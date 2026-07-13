package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"chatchain/internal/markdown"
	"chatchain/internal/ui"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"
)

// RunV2 is the --ui=v2 interactive loop: the same turn engine and session
// semantics as Run, rendered through the bubbletea facade (internal/ui)
// instead of the readline REPL. It is a deliberate FORK of Run for the
// migration window (docs/design/ui-architecture.md P2): pure logic helpers are
// shared, the ~duplicated loop shape dies with the old stack at P5.
//
// v2-path invariants: after ui.New() nothing may write to the terminal except
// through the facade — no promptui, no spinner, no raw OSC/ANSI, no readline.
func RunV2(p provider.Provider, systemPrompt string, importedHistory []provider.Message, dispatch tool.Dispatcher, mgr *mcpmgr.Manager, sw *SessionWriter, contextWindow int, agent AgentOptions, reqLog *RequestLog) error {
	// ---- pre-Program phase: plain stdout, the Program hasn't claimed the
	// terminal yet. The OSC background query MUST happen here (during the
	// Program it would race the event loop's stdin ownership).
	detectCodeTheme()

	var overlay *systemOverlay
	if agent.Enabled {
		cwd, cerr := os.Getwd()
		if cerr != nil || cwd == "" {
			cwd = agent.Root
		}
		overlay = newSystemOverlay(agent.Root, cwd)
	}
	setAgentCommands(agent.Enabled)
	sessionScope := ""
	if agent.Enabled {
		sessionScope = agent.Root
	}

	var history []provider.Message
	persisted := 0
	if len(importedHistory) > 0 {
		history = importedHistory
		persisted = len(history)
	} else if systemPrompt != "" {
		history = append(history, provider.Message{Role: "system", Content: systemPrompt})
	}

	budget := newContextBudget(contextWindow)
	if len(history) > 0 {
		budget.update(p, history)
	}
	if sw != nil && !sw.created {
		sw.SetContextWindow(budget.window)
	}

	DimStyle.Println("Chat started (--ui=v2 preview). Press Ctrl+C to exit.")
	DimStyle.Println("Commands: /file [path], /session, /model, /compact, /export, /status, /tools, /debug" + agentCommandHint(overlay))
	if id := sw.ID(); id != "" {
		DimStyle.Printf("Session: %s\n", id)
	}
	if n := overlay.fileCount(); n > 0 {
		DimStyle.Printf("Agent mode: AGENTS.md loaded (%d files, %.1f KB)\n", n, float64(overlay.chainSize())/1024)
	}
	if n := overlay.skillCount(); n > 0 {
		DimStyle.Printf("Agent mode: %d skill(s) available\n", n)
	}
	for _, warn := range overlay.warnings() {
		DimStyle.Printf("⚠ %s\n", warn)
	}
	if len(importedHistory) > 0 {
		if msgs := lastRounds(history, resumeEchoRounds); len(msgs) > 0 {
			fmt.Println()
			echoRounds(os.Stdout, msgs)
		}
	}
	fmt.Println()

	// ---- Program phase: the facade owns the terminal from here on.
	u := ui.New()
	defer u.Close()
	defer func() { sw.Close() }() // sw may be swapped by /session
	u.SetTitle(sw.Title())

	pushStatus := func() {
		model := p.Model()
		if model == "" {
			model = p.Type()
		}
		u.SetStatus(ui.StatusData{Model: model, CtxUsed: budget.used, CtxWindow: budget.window, Estimated: !budget.haveUsage})
	}
	pushStatus()

	if mgr != nil {
		go reportMCPFailuresV2(mgr, u)
	}

	ctx := context.Background()
	tp, isToolProvider := p.(provider.ToolProvider)
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}
	var pendingAttachments []provider.Attachment

	printErr := func(format string, a ...any) { u.PrintLines(ErrorStyle.Sprintf(format, a...)) }
	printDim := func(format string, a ...any) { u.PrintLines(DimStyle.Sprintf(format, a...)) }

	var titleWG sync.WaitGroup
	titled := len(importedHistory) > 0

	persistTurn := func() {
		if sw != nil && persisted < len(history) {
			if err := sw.AppendMessages(history[persisted:]); err != nil {
				printErr("Warning: failed to save session: %v", err)
				return
			}
			persisted = len(history)
		}
	}
	maybeTitle := func() {
		if titled || sw == nil {
			return
		}
		var firstUser, firstAssistant string
		for _, m := range history {
			if firstUser == "" && m.Role == "user" {
				firstUser = m.Content
			}
			if firstAssistant == "" && m.Role == "assistant" && m.Content != "" {
				firstAssistant = m.Content
			}
		}
		if firstUser == "" || firstAssistant == "" {
			return
		}
		titled = true
		placeholder := truncateRunes(strings.TrimSpace(firstUser), 40)
		sw.SetTitle(placeholder)
		u.SetTitle(placeholder)
		titleWG.Add(1)
		go func(fu, fa string, target *SessionWriter) {
			defer titleWG.Done()
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if title := generateTitleText(tctx, p, fu, fa, target); title != "" {
				u.SetTitle(title)
			}
		}(firstUser, firstAssistant, sw)
	}

	compactDeclined := 0
	compactNow := func(hint string, manual bool) {
		stop := u.Busy("Compacting context…")
		newHist, summary, retainTail, changed, cerr := compactHistory(ctx, p, history, hint)
		stop()
		if cerr != nil {
			printErr("Compaction failed: %v", cerr)
			return
		}
		if !changed {
			if manual {
				printDim("Nothing to compact yet.")
			}
			return
		}
		history = newHist
		if err := sw.AppendCompaction(summary, retainTail); err != nil {
			printErr("Warning: failed to persist compaction marker: %v", err)
		}
		persisted = len(history)
		budget.reseed(history)
		compactDeclined = 0
		printDim("Context compacted → %s", budget.status())
		pushStatus()
	}

	interruptTurn := func(watermark int, partial, partialReasoning string) {
		var persist bool
		history, persist = finalizeInterrupt(history, watermark, partial, partialReasoning)
		u.PrintLines("", DimStyle.Sprint("Interrupted."), "")
		if persist {
			persistTurn()
			maybeTitle()
		}
		budget.update(p, history)
		pushStatus()
	}

	// retryV2 mirrors retryWithCountdown without the \r countdown (the frame
	// spinner shows the wait instead).
	retryV2 := func(fn func() error) error {
		err := fn()
		if err == nil || !isRetryable(err) {
			return err
		}
		for attempt := 1; attempt <= maxRetries; attempt++ {
			printErr("Error: %v", err)
			stop := u.Busy(fmt.Sprintf("Rate limited — retrying (attempt %d/%d)", attempt, maxRetries))
			time.Sleep(time.Duration(attempt) * time.Second)
			stop()
			err = fn()
			if err == nil || !isRetryable(err) {
				return err
			}
		}
		return err
	}

	for {
		in, rerr := u.ReadInput(ctx)
		if rerr != nil { // ErrInterrupted (idle Ctrl+C/D), ErrClosed, ctx
			titleWG.Wait()
			return nil
		}
		input := strings.TrimSpace(in.Text)
		if input == "" {
			continue
		}
		if !isReadOnlyViewer(input) {
			titleWG.Wait()
		}

		// ---- commands (minimal v2 surfaces; fidelity lands in P3) ----
		if input == "/file" || strings.HasPrefix(input, "/file ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/file"))
			if path == "" {
				printDim("v2: usage /file <path> (the browser/removal picker lands in P3)")
				continue
			}
			att, aerr := ReadAttachment(path)
			if aerr != nil {
				printErr("Error: %v", aerr)
			} else {
				pendingAttachments = append(pendingAttachments, att)
				printDim("Attached: %s (%s, %d bytes)", att.Filename, att.MimeType, len(att.Data))
			}
			continue
		}
		if input == "/model" || strings.HasPrefix(input, "/model ") {
			stop := u.Busy("Fetching available models...")
			models, ferr := p.ListModels(ctx)
			stop()
			if ferr != nil {
				printErr("Error: %v", ferr)
				continue
			}
			cursor := 0
			for i, m := range models {
				if m == p.Model() {
					cursor = i
				}
			}
			r, serr := u.Select(ctx, ui.SelectSpec{Title: "/model", Items: models, Cursor: cursor})
			if serr != nil || r.Cancelled {
				continue
			}
			p.SetModel(models[r.Index])
			sw.SetModel(models[r.Index])
			printDim("Model switched to %s", models[r.Index])
			pushStatus()
			continue
		}
		if input == "/session" || strings.HasPrefix(input, "/session ") {
			infos, lerr := ListSessions(sessionScope)
			if lerr != nil {
				printErr("Error: %v", lerr)
				continue
			}
			if len(infos) == 0 {
				printDim("No sessions yet.")
				continue
			}
			items := make([]string, len(infos))
			for i, s := range infos {
				items[i] = sessionLabel(s)
			}
			r, serr := u.Select(ctx, ui.SelectSpec{Title: "/session — resume", Items: items})
			if serr != nil || r.Cancelled {
				continue
			}
			id := infos[r.Index].ID
			if id == sw.ID() {
				printDim("Already in this session.")
				continue
			}
			newSW, sess, lerr2 := ResumeSession(id, p)
			if lerr2 != nil {
				printErr("Error: %v", lerr2)
				continue
			}
			sw.Close()
			sw = newSW
			u.SetTitle(sw.Title())
			history = sess.Messages
			persisted = len(history)
			budget.reseed(history)
			if ur, ok := p.(interface{ ResetUsage() }); ok {
				ur.ResetUsage()
			}
			pendingAttachments = nil
			titled = true
			if sess.Meta.Provider == p.Type() && sess.Meta.Model != "" {
				p.SetModel(sess.Meta.Model)
			}
			ApplySessionTuning(sess, p, false, false, budget.setWindow)
			printDim("Resumed session %s (%d messages)", id, len(history))
			if msgs := lastRounds(history, resumeEchoRounds); len(msgs) > 0 {
				var buf strings.Builder
				echoRounds(&buf, msgs)
				u.PrintLines("")
				u.PrintLines(strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)
			}
			pushStatus()
			continue
		}
		if input == "/compact" || strings.HasPrefix(input, "/compact ") {
			hint := strings.TrimSpace(strings.TrimPrefix(input, "/compact"))
			compactNow(hint, true)
			continue
		}
		if input == "/export" || strings.HasPrefix(input, "/export ") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/export"))
			if arg == "" {
				printDim("v2: usage /export <file.html|file.md> (the format picker lands in P3)")
				continue
			}
			lc := &lineCommitter{commit: u.PrintLines}
			exportChat(lc, arg, sw, history, p)
			lc.flush()
			continue
		}
		if input == "/tools" || strings.HasPrefix(input, "/tools ") {
			lines := toolStatusLines(dispatch, mgr)
			lines = append(lines, "")
			lines = append(lines, mcpStatusLines(mgr)...)
			_ = u.View(ctx, ui.ViewSpec{Title: "/tools", Lines: lines})
			continue
		}
		if input == "/debug" || strings.HasPrefix(input, "/debug ") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/debug"))
			switch arg {
			case "on":
				reqLog.SetVerbose(true)
				printDim("Request recording ON")
				continue
			case "off":
				reqLog.SetVerbose(false)
				printDim("Request recording OFF")
				continue
			}
			state := "off"
			if reqLog.Verbose() {
				state = "on"
			}
			lines := append([]string{DimStyle.Sprintf("recording: %s (/debug on|off)", state), ""}, requestRows(reqLog.Entries())...)
			_ = u.View(ctx, ui.ViewSpec{Title: "/debug", Lines: lines})
			continue
		}
		if overlay != nil && (input == "/skills" || strings.HasPrefix(input, "/skills ")) {
			overlay.refresh()
			_ = u.View(ctx, ui.ViewSpec{Title: "/skills", Lines: skillsStatusLines(overlay.skillList(), overlay.warnings(), agent.Root)})
			continue
		}
		if input == "/status" || strings.HasPrefix(input, "/status ") {
			items := statusLines(p, budget, history, len(pendingAttachments), dispatch, mgr, sw)
			lines := make([]string, len(items))
			for i, it := range items {
				lines[i] = fmt.Sprintf("%s  %s", BoldStyle.Sprintf("%-12s", it.Name), it.Value)
			}
			_ = u.View(ctx, ui.ViewSpec{Title: "/status", Lines: lines})
			continue
		}

		// ---- message path ----
		u.UserBlock(in.Display)

		if p.Model() == "" && !ensureModelV2(ctx, u, p, sw) {
			continue
		}

		agentsChanged, skillsChanged := overlay.refresh()
		if agentsChanged {
			printDim("AGENTS.md reloaded (%d files)", overlay.fileCount())
		}
		if skillsChanged {
			printDim("Skills reloaded (%d skill(s))", overlay.skillCount())
			for _, warn := range overlay.warnings() {
				printDim("⚠ %s", warn)
			}
		}
		sendOverlay := overlay.content()

		extra := budget.counter.count(input)
		for _, att := range pendingAttachments {
			extra += len(att.Data) / 1000
		}
		if budget.shouldOfferCompact(extra, compactDeclined) {
			ok, cerr := u.Confirm(ctx, fmt.Sprintf("Context %s — compact before sending?", budget.status()), "Compact now", "Not now")
			if cerr == nil && ok {
				compactNow("", false)
			} else {
				compactDeclined = budget.used + extra
			}
		}

		history = append(history, provider.Message{Role: "user", Content: input, Attachments: pendingAttachments})
		pendingAttachments = nil
		hist0 := len(history)

		if dispatch != nil {
			tools = dispatch.Tools()
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		sink := u.StartStream(cancelTurn)
		var reply, thinking string
		retryErr := retryV2(func() error {
			history = history[:hist0] // tool rounds append; reset per attempt
			var err error
			if isToolProvider && len(tools) > 0 {
				reply, thinking, err = toolLoopV2(turnCtx, u, sink, tp, dispatch, &history, tools, sendOverlay)
			} else {
				reply, thinking, err = streamTurnV2(turnCtx, u, sink, func(w io.Writer, r io.WriteCloser) (string, string, error) {
					return p.StreamChat(turnCtx, composeSendHistory(history, sendOverlay), w, r)
				})
			}
			return err
		})
		sink.Done() // closes any leaked preview + pops the turn scope
		cancelTurn()

		if errors.Is(retryErr, errInterrupted) {
			interruptTurn(hist0-1, reply, thinking)
			continue
		}
		if retryErr != nil {
			printErr("Error: %v", retryErr)
			u.PrintLines("")
			history = history[:hist0-1]
			continue
		}

		u.PrintLines("")
		history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
		persistTurn()
		budget.update(p, history)
		pushStatus()
		maybeTitle()
	}
}

// ensureModelV2 is the lazy model picker for the v2 path.
func ensureModelV2(ctx context.Context, u *ui.UI, p provider.Provider, sw *SessionWriter) bool {
	stop := u.Busy("Fetching available models...")
	models, err := p.ListModels(ctx)
	stop()
	if err != nil {
		u.PrintLines(ErrorStyle.Sprintf("Error: %v", err))
		return false
	}
	if len(models) == 0 {
		u.PrintLines(ErrorStyle.Sprint("No models available"))
		return false
	}
	r, serr := u.Select(ctx, ui.SelectSpec{Title: "Select a model", Items: models})
	if serr != nil || r.Cancelled {
		return false
	}
	p.SetModel(models[r.Index])
	sw.SetModel(models[r.Index])
	u.PrintLines(DimStyle.Sprintf("Using model: %s", models[r.Index]))
	return true
}

// reportMCPFailuresV2 relays background MCP connect failures into scrollback,
// stopping when the Program exits.
func reportMCPFailuresV2(mgr *mcpmgr.Manager, u *ui.UI) {
	events := mgr.Events()
	for {
		select {
		case s, ok := <-events:
			if !ok {
				return
			}
			if s.Connected {
				continue
			}
			u.PrintLines(ErrorStyle.Sprintf("⚠ MCP %s failed: %s", s.Name, strings.SplitN(s.Err, "\n", 2)[0]))
		case <-u.Done():
			return
		}
	}
}

// reasoningV2 renders streaming reasoning as a frame preview (rolling window)
// collapsing to a "◇ thought for Ns" marker in scrollback — the v2 twin of
// reasoningStream.
type reasoningV2 struct {
	pw    io.WriteCloser
	sink  ui.StreamSink
	start time.Time
	done  bool
}

func newReasoningV2(sink ui.StreamSink) *reasoningV2 {
	return &reasoningV2{pw: sink.BlockPreview("Thinking"), sink: sink, start: time.Now()}
}

func (r *reasoningV2) Write(p []byte) (int, error) {
	if r.pw == nil {
		return len(p), nil
	}
	return r.pw.Write(p)
}

func (r *reasoningV2) finish() {
	if r.done {
		return
	}
	r.done = true
	if r.pw != nil {
		r.pw.Close()
	}
	r.sink.CommitLines(dim(fmt.Sprintf("%s thought for %s", reasoningSymbol, reasoningElapsed(r.start))))
}

// streamTurnV2 renders one provider stream through the ui sink: busy spinner
// until the first token, the reasoning rolling window, then markdown-rendered
// content committed line by line. call runs the provider against the given
// content/reasoning writers and returns its final (reply, thinking, err).
// A ctx cancel (ESC/Ctrl+C via the ui scope) maps to errInterrupted with the
// partials the user actually saw — same contract as streamResponse.
func streamTurnV2(ctx context.Context, u *ui.UI, sink ui.StreamSink, call func(w io.Writer, r io.WriteCloser) (string, string, error)) (string, string, error) {
	reasonPr, reasonPw := io.Pipe()
	contentPr, contentPw := io.Pipe()
	var reply, thinking string
	var streamErr error
	var contentBuf, reasonBuf strings.Builder
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer contentPw.Close()
		defer reasonPw.Close()
		reply, thinking, streamErr = call(
			io.MultiWriter(contentPw, &contentBuf),
			teeWriteCloser{io.MultiWriter(reasonPw, &reasonBuf), reasonPw})
	}()

	interrupted := func() bool { return ctx.Err() != nil }
	fail := func(err error) (string, string, error) {
		if interrupted() {
			return contentBuf.String(), reasonBuf.String(), errInterrupted
		}
		return "", "", err
	}

	firstChunk := make([]byte, 4096)
	var firstN int
	var readErr error
	hasReasoning := false

	sink.CommitLines("") // blank line opening the assistant's turn

	stop := u.Busy("Thinking...")
	firstN, readErr = reasonPr.Read(firstChunk)
	if readErr != nil {
		readErr = nil
		firstN, readErr = contentPr.Read(firstChunk)
	} else {
		hasReasoning = true
	}
	stop()

	if readErr != nil {
		<-done
		if streamErr != nil {
			return fail(streamErr)
		}
		return fail(readErr)
	}

	newContent := func() (*markdown.Writer, *uiMDSink) {
		msink := newUIMDSink(sink, u.Width)
		return markdown.NewWriter(msink), msink
	}

	if hasReasoning {
		rv := newReasoningV2(sink)
		finishRV := func() { rv.finish() } // done-guarded inside
		defer finishRV()
		rv.Write(firstChunk[:firstN])
		io.Copy(rv, reasonPr)
		finishRV()

		firstN, readErr = contentPr.Read(firstChunk)
		if readErr != nil {
			<-done
			if interrupted() || streamErr != nil {
				return fail(streamErr)
			}
			// Reasoning-only response: render the reasoning as the answer.
			sink.CommitLines("")
			mdw, msink := newContent()
			mdw.Write([]byte(thinking))
			mdw.Flush()
			msink.flush()
			return thinking, thinking, nil
		}
		sink.CommitLines("") // blank line separating reasoning from the reply
	}

	mdw, msink := newContent()
	mdw.Write(firstChunk[:firstN])
	io.Copy(mdw, contentPr)
	mdw.Flush()
	msink.flush()
	<-done

	if interrupted() || streamErr != nil {
		return fail(streamErr)
	}
	return reply, thinking, nil
}

// toolLoopV2 is the v2 twin of executeWithTools: rounds of streaming +
// tool execution rendered through the ui frame (Busy labels instead of the
// stderr spinner; per-tool cancel scopes instead of raw-mode watches).
func toolLoopV2(ctx context.Context, u *ui.UI, sink ui.StreamSink, tp provider.ToolProvider, dispatch tool.Dispatcher, history *[]provider.Message, tools []provider.ToolDef, overlay string) (string, string, error) {
	for rounds := 0; ; rounds++ {
		if rounds == maxToolRounds {
			return "", "", errToolRoundsExceeded
		}
		content, reasoning, toolCalls, err := streamToolRoundV2(ctx, u, sink, tp, composeSendHistory(*history, overlay), tools)
		if err != nil {
			return content, reasoning, err
		}
		if len(toolCalls) == 0 {
			return content, reasoning, nil
		}

		msg := provider.Message{Role: "assistant", Content: content, ToolCalls: toolCalls}
		if rcp, ok := tp.(provider.RawContentProvider); ok {
			msg.RawContent = rcp.LastRawContent()
		}
		*history = append(*history, msg)

		for _, tc := range toolCalls {
			sink.CommitLines("", CodeStyle.Sprint(toolCallHeader(tc)))

			toolCtx, cancel := context.WithCancel(ctx)
			pop := u.PushCancelScope(cancel)
			stop := u.Busy(displayToolName(tc.Name))
			resultText, isError, callErr := dispatch.CallTool(toolCtx, tc.Name, tc.Arguments)
			stop()
			pop()
			cancel()
			if callErr != nil {
				resultText = fmt.Sprintf("Error calling tool: %v", callErr)
				isError = true
			}

			lc := &lineCommitter{commit: sink.CommitLines}
			printToolResult(lc, resultText, isError)
			lc.flush()

			*history = append(*history, provider.Message{
				Role:         "tool",
				Content:      resultText,
				ToolCallID:   tc.ID,
				ToolCallName: tc.Name,
				IsError:      isError,
			})

			if ctx.Err() != nil { // tool cancelled via ESC propagates as interrupt
				return "", "", errInterrupted
			}
		}
	}
}

// streamToolRoundV2 is the v2 twin of streamToolRound (interactive only —
// Once keeps the v1 quiet path).
func streamToolRoundV2(ctx context.Context, u *ui.UI, sink ui.StreamSink, tp provider.ToolProvider, history []provider.Message, tools []provider.ToolDef) (string, string, []provider.ToolCall, error) {
	reasonPr, reasonPw := io.Pipe()
	contentPr, contentPw := io.Pipe()
	var content, reasoning string
	var toolCalls []provider.ToolCall
	var streamErr error
	var contentBuf, reasonBuf strings.Builder
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer contentPw.Close()
		defer reasonPw.Close()
		content, reasoning, toolCalls, streamErr = tp.StreamChatWithTools(ctx, history, tools,
			io.MultiWriter(contentPw, &contentBuf),
			teeWriteCloser{io.MultiWriter(reasonPw, &reasonBuf), reasonPw})
	}()

	interrupted := func() bool { return ctx.Err() != nil }
	fail := func(err error) (string, string, []provider.ToolCall, error) {
		if interrupted() {
			return contentBuf.String(), reasonBuf.String(), nil, errInterrupted
		}
		return "", "", nil, err
	}

	firstChunk := make([]byte, 4096)
	var firstN int
	var readErr error
	hasReasoning := false

	sink.CommitLines("") // blank line opening this round

	stop := u.Busy("Thinking...")
	firstN, readErr = reasonPr.Read(firstChunk)
	if readErr != nil {
		readErr = nil
		firstN, readErr = contentPr.Read(firstChunk)
	} else {
		hasReasoning = true
	}
	stop()

	if readErr != nil {
		<-done
		if interrupted() || streamErr != nil {
			return fail(streamErr)
		}
		if len(toolCalls) > 0 { // tool calls with no text
			return content, reasoning, toolCalls, nil
		}
		return fail(readErr)
	}

	newContent := func() (*markdown.Writer, *uiMDSink) {
		msink := newUIMDSink(sink, u.Width)
		return markdown.NewWriter(msink), msink
	}

	if hasReasoning {
		rv := newReasoningV2(sink)
		finishRV := func() { rv.finish() }
		defer finishRV()
		rv.Write(firstChunk[:firstN])
		io.Copy(rv, reasonPr)
		finishRV()

		firstN, readErr = contentPr.Read(firstChunk)
		if readErr != nil {
			<-done
			if interrupted() || streamErr != nil {
				return fail(streamErr)
			}
			if len(toolCalls) > 0 {
				return content, reasoning, toolCalls, nil
			}
			// Reasoning-only response: render the reasoning as the answer.
			sink.CommitLines("")
			mdw, msink := newContent()
			mdw.Write([]byte(reasoning))
			mdw.Flush()
			msink.flush()
			return reasoning, reasoning, nil, nil
		}
		sink.CommitLines("")
	}

	if firstN > 0 {
		mdw, msink := newContent()
		mdw.Write(firstChunk[:firstN])
		io.Copy(mdw, contentPr)
		mdw.Flush()
		msink.flush()
	} else {
		io.Copy(io.Discard, contentPr)
	}
	<-done

	if interrupted() || streamErr != nil {
		return fail(streamErr)
	}
	return content, reasoning, toolCalls, nil
}
