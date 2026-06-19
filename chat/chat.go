package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chatchain/internal/promptui"
	"chatchain/internal/readline"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"

	"github.com/briandowns/spinner"
	"golang.org/x/term"
)

// runSelect shows a single-select menu over the string items using promptui's
// clean defaults and native ESC/q/Ctrl+C cancel. It returns the chosen index with
// ok=true, or (0, false) when the user cancels — the single-select sibling of
// multiSelect.
func runSelect(label string, items []string, size int) (int, bool) {
	prompt := promptui.Select{
		Label:        label,
		Items:        items,
		Size:         size,
		HideSelected: true,
	}
	idx, _, err := prompt.Run()
	if err != nil {
		return 0, false
	}
	return idx, true
}

// pickContextWindow shows a selector of common context window sizes (ESC to
// cancel → returns 0). The label header shows current usage so the user can
// judge. Industry max is currently 1M.
func pickContextWindow(b *contextBudget) (int, error) {
	vals := []int{8_000, 32_000, 128_000, 200_000, 256_000, 1_000_000}
	labels := make([]string, len(vals))
	for i, v := range vals {
		labels[i] = formatTokens(v)
		if v == b.window {
			labels[i] += " (current)"
		}
	}
	idx, ok := runSelect(fmt.Sprintf("Context window — now %s", b.status()), labels, 10)
	if !ok {
		return 0, nil // cancelled (ESC / q / Ctrl+C)
	}
	return vals[idx], nil
}

// SelectModel prompts for a model. On user cancel (ESC / q / Ctrl+C / Ctrl+D) it
// returns ("", nil) — empty, not an error.
func SelectModel(models []string) (string, error) {
	idx, ok := runSelect("Select a model", models, 15)
	if !ok {
		return "", nil // cancelled (ESC / q / Ctrl+C / EOF)
	}
	return models[idx], nil
}

// reserveBottomLines guarantees at least n empty rows below the current
// cursor position, scrolling the screen up if necessary. macOS Terminal.app
// crashes (and ergochat/readline gets confused) when CJK IME composition
// triggers line wrap at the absolute bottom row, so we keep the prompt away
// from that edge.
//
// Uses IND (ESC D) instead of LF so the cursor column is preserved — IND
// scrolls when at the bottom margin but only moves down otherwise; CUU
// (ESC [ n A) never scrolls. Net effect: no-op when there is already enough
// headroom, otherwise scroll the deficit and return the cursor to the same
// logical position.
func reserveBottomLines(n int) {
	if n <= 0 {
		return
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("\033D")
	}
	fmt.Fprintf(&b, "\033[%dA", n)
	os.Stdout.WriteString(b.String())
}

// bottomReserveListener returns a readline.Listener that recomputes the
// required bottom headroom on every keystroke based on the current buffer
// length and terminal width. Called both at prompt init (with line=nil) and
// after each rune is written to the buffer.
func bottomReserveListener() readline.Listener {
	return func(line []rune, pos int, key rune) ([]rune, int, bool) {
		w, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || w <= 0 {
			w = 80
		}
		// Worst-case column estimate: every rune as 2 cols (CJK), plus
		// a fixed allowance for the visible prompt.
		cols := len(line)*2 + 8
		lines := cols/w + 4
		if lines > 40 {
			lines = 40
		}
		reserveBottomLines(lines)
		return nil, 0, false
	}
}

// WithSpinner runs action while showing a single-line spinner with the given
// title, then stops the spinner. Used for blocking waits (model listing, MCP
// connect join) so the user sees activity instead of a blank screen.
func WithSpinner(title string, action func()) {
	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Suffix = " " + title
	s.Start()
	action()
	s.Stop()
}

func FetchModels(ctx context.Context, p provider.Provider) ([]string, error) {
	var models []string
	var fetchErr error

	WithSpinner("Fetching available models...", func() {
		models, fetchErr = p.ListModels(ctx)
	})

	return models, fetchErr
}

func Once(ctx context.Context, p provider.Provider, message string, systemPrompt string, dispatch tool.Dispatcher, w io.Writer) error {
	var messages []provider.Message
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, provider.Message{Role: "user", Content: message})

	tp, isToolProvider := p.(provider.ToolProvider)
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}

	if isToolProvider && len(tools) > 0 {
		reply, _, err := executeWithTools(ctx, tp, dispatch, &messages, tools, w, true)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, reply)
		return nil
	}

	reply, err := p.Chat(ctx, messages)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, reply)
	return nil
}

func ReadSystemPrompt(w io.Writer) (string, error) {
	pf := &pasteFilter{r: os.Stdin}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          BoldStyle.Sprint("System> "),
		InterruptPrompt: "^C",
		Stdin:           pf,
		Listener:        bottomReserveListener(),
	})
	if err != nil {
		return "", err
	}
	defer rl.Close()

	os.Stdout.WriteString("\033[?2004h")
	defer os.Stdout.WriteString("\033[?2004l")

	input, err := rl.Readline()
	if err != nil {
		return "", nil // skip on Ctrl+C / EOF
	}
	return expandPasteTags(strings.TrimSpace(input), pf), nil
}

type chatCompleter struct{}

func (c *chatCompleter) Do(line []rune, pos int) ([][]rune, int) {
	text := string(line[:pos])

	// Only complete lines starting with /
	if !strings.HasPrefix(text, "/") {
		return nil, 0
	}

	// Command completion (no space yet)
	if !strings.Contains(text, " ") {
		var candidates [][]rune
		for _, cmd := range slashCommands {
			full := cmd + " " // insert a trailing space, ready for args
			if strings.HasPrefix(full, text) {
				candidates = append(candidates, []rune(full[len(text):]))
			}
		}
		return candidates, len([]rune(text))
	}

	// File path completion for "/file "
	if strings.HasPrefix(text, "/file ") && !strings.HasPrefix(text, "/files") {
		return completeFilePath(text[6:])
	}

	return nil, 0
}

func completeFilePath(path string) ([][]rune, int) {
	if path == "" {
		path = "./"
	}

	// Expand ~
	expandedPath := path
	if strings.HasPrefix(expandedPath, "~/") {
		home, _ := os.UserHomeDir()
		if home != "" {
			expandedPath = filepath.Join(home, expandedPath[2:])
		}
	}

	var dir, partial string
	if strings.HasSuffix(path, "/") {
		dir = expandedPath
		partial = ""
	} else {
		dir = filepath.Dir(expandedPath)
		partial = filepath.Base(expandedPath)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}

	// Collect matching candidates as suffixes
	var candidates [][]rune
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(name, partial) {
			continue
		}
		suffix := name[len(partial):]
		if e.IsDir() {
			suffix += "/"
		} else {
			suffix += " "
		}
		candidates = append(candidates, []rune(suffix))
	}

	// Cap candidates to fit terminal (prevents flooding)
	maxItems := calcMaxItems(candidates, partial)
	if len(candidates) > maxItems && maxItems > 0 {
		candidates = candidates[:maxItems]
	}

	return candidates, len([]rune(partial))
}

func calcMaxItems(candidates [][]rune, partial string) int {
	tw, th, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || tw <= 0 || th <= 0 {
		tw, th = 80, 24
	}

	maxWidth := 0
	for _, c := range candidates {
		w := len(partial) + len(c)
		if w > maxWidth {
			maxWidth = w
		}
	}
	colWidth := maxWidth + 2
	if colWidth > tw {
		colWidth = tw
	}
	if colWidth < 1 {
		colWidth = 1
	}

	colNum := (tw - 1) / colWidth
	if colNum < 1 {
		colNum = 1
	}

	maxRows := th / 3
	if maxRows < 3 {
		maxRows = 3
	}

	return maxRows * colNum
}

// expandPasteTags finds paste preview tags like [#1 foo... 5 lines]
// in the input and replaces them with the actual pasted content.
func expandPasteTags(input string, pf *pasteFilter) string {
	for {
		start := strings.Index(input, "[#")
		if start < 0 {
			break
		}
		end := strings.Index(input[start:], "]")
		if end < 0 {
			break
		}
		end += start

		tag := input[start+1 : end] // e.g. "#1 Hello world... 5 lines"
		// Extract the #N prefix to look up the paste.
		if spaceIdx := strings.Index(tag, " "); spaceIdx > 0 {
			tagKey := tag[:spaceIdx+1] // "#1 "
			if text := pf.ConsumePaste(tagKey); text != "" {
				input = input[:start] + text + input[end+1:]
				continue
			}
		}
		// Not a paste tag or not found — skip past it.
		break
	}
	return input
}

const maxRetries = 10

// http4xxPattern matches HTTP 4xx status codes (except 429) in error messages.
var http4xxPattern = regexp.MustCompile(`\b4\d{2}\b`)

// isRetryable returns true if the error is likely transient and worth retrying.
// Non-retryable: io.EOF, HTTP 4xx (except 429 rate limit).
func isRetryable(err error) bool {
	if err == io.EOF {
		return false
	}
	msg := err.Error()
	matches := http4xxPattern.FindAllString(msg, -1)
	for _, m := range matches {
		if m != "429" {
			return false
		}
	}
	return true
}

func retryWithCountdown(w io.Writer, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	if !isRetryable(err) {
		return err
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ErrorStyle.Fprintf(w, "Error: %v\n", err)
		for sec := attempt; sec > 0; sec-- {
			fmt.Fprintf(w, "\r\033[K")
			DimStyle.Fprintf(w, "Retrying in %ds... (attempt %d/%d)", sec, attempt, maxRetries)
			time.Sleep(1 * time.Second)
		}
		fmt.Fprintf(w, "\r\033[K")
		err = fn()
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
	}
	return err
}

func Run(p provider.Provider, systemPrompt string, importedHistory []provider.Message, dispatch tool.Dispatcher, mgr *mcpmgr.Manager, sw *SessionWriter, contextWindow int, w io.Writer) error {
	pf := &pasteFilter{r: os.Stdin}
	// lineEmpty tracks whether the input line is empty; the Listener keeps it in
	// sync with readline's real buffer and the reader resets it on Enter. A
	// line-leading "/" then auto-opens the command menu, and the Painter colors
	// the command token live as it is typed.
	var lineEmpty atomic.Bool
	lineEmpty.Store(true)
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          UserStyle.Sprint("You> "),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		AutoComplete:    &chatCompleter{},
		Stdin:           newSlashTriggerReader(pf, &lineEmpty),
		Painter:         commandPainter,
		Listener:        slashTriggerListener(&lineEmpty, bottomReserveListener()),
	})
	if err != nil {
		return fmt.Errorf("failed to initialize input: %w", err)
	}
	defer rl.Close()
	// Own the session writer's lifecycle here: it may be swapped by /session,
	// so close whatever it points to at return (closure over the variable).
	defer func() { sw.Close() }()

	// Enable bracketed paste mode AFTER readline init, so readline's
	// terminal setup doesn't override it.
	os.Stdout.WriteString("\033[?2004h")
	defer os.Stdout.WriteString("\033[?2004l")

	var history []provider.Message
	// persisted = number of leading history messages already written to the
	// session. We persist only committed turns (after a successful response),
	// so a failed/retried turn that rolls back history never reaches disk.
	persisted := 0
	if len(importedHistory) > 0 {
		history = importedHistory
		persisted = len(history)
	} else if systemPrompt != "" {
		// Keep the system message in memory only; persisted stays 0 so it is
		// written with the first real turn. A command-only session that never
		// reaches a turn thus creates nothing on disk.
		history = append(history, provider.Message{Role: "system", Content: systemPrompt})
	}
	ctx := context.Background()

	budget := newContextBudget(contextWindow)
	if len(history) > 0 {
		budget.update(p, history) // seed from loaded history on resume
	}

	DimStyle.Fprintln(w, "Chat started. Press Ctrl+C to exit.")
	DimStyle.Fprintln(w, "Commands: /file [path], /files, /session, /sessions, /model, /context, /compact, /status, /mcp, /tools")
	if id := sw.ID(); id != "" {
		DimStyle.Fprintf(w, "Session: %s\n", id)
	}
	fmt.Fprintln(w)

	tp, isToolProvider := p.(provider.ToolProvider)
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}

	var pendingAttachments []provider.Attachment

	// Auto-title: after the first completed turn, generate a short title in the
	// background (one provider.Chat call). titleWG serializes that goroutine
	// against the next turn's provider access so there is no concurrent use of
	// the provider or session writer.
	var titleWG sync.WaitGroup
	titled := len(importedHistory) > 0 // resumed sessions already have a title

	persistTurn := func() {
		if sw != nil && persisted < len(history) {
			if err := sw.AppendMessages(history[persisted:]); err != nil {
				// Keep persisted where it is so the unsaved tail is retried on
				// the next turn instead of being silently dropped.
				ErrorStyle.Fprintf(w, "Warning: failed to save session: %v\n", err)
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
		// Immediate placeholder so the session is identifiable right away.
		sw.SetTitle(truncateRunes(strings.TrimSpace(firstUser), 40))
		titleWG.Add(1)
		go func(u, a string, target *SessionWriter) {
			defer titleWG.Done()
			// Bound the background request so a hung provider can't make
			// titleWG.Wait() block exit indefinitely.
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			generateTitle(tctx, p, u, a, target)
		}(firstUser, firstAssistant, sw)
	}

	// compactNow summarizes the older history into one summary (LLM call under a
	// spinner), swaps in the compacted view, writes an Event Store marker, and
	// re-seeds the budget. persisted is reset to the new view length — everything
	// in it is already on disk (originals + the marker), only later turns append.
	compactNow := func(hint string, manual bool) {
		var newHist []provider.Message
		var summary string
		var retainTail int
		var changed bool
		var cerr error
		WithSpinner("Compacting context…", func() {
			newHist, summary, retainTail, changed, cerr = compactHistory(ctx, p, history, hint)
		})
		if cerr != nil {
			ErrorStyle.Fprintf(w, "Compaction failed: %v\n", cerr)
			return
		}
		if !changed {
			if manual {
				DimStyle.Fprintln(w, "Nothing to compact yet.")
			}
			return
		}
		history = newHist
		if err := sw.AppendCompaction(summary, retainTail); err != nil {
			// The in-memory view is already compacted; the full log is still on
			// disk, so resume recovers the (uncompacted) history — only this
			// summary marker is lost. Warn rather than fail the turn.
			ErrorStyle.Fprintf(w, "Warning: failed to persist compaction marker: %v\n", err)
		}
		persisted = len(history)
		budget.reseed(history)
		DimStyle.Fprintf(w, "Context compacted → %s\n", budget.status())
	}

	for {
		input, err := rl.Readline()
		if err != nil { // io.EOF or readline.ErrInterrupt
			titleWG.Wait()
			fmt.Fprintln(w, "\nBye!")
			return nil
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		// Expand paste tags: [#1 first few chars... N lines] → full pasted text
		input = expandPasteTags(input, pf)

		// Wait for any in-flight title generation before touching the provider
		// or session writer, so the background goroutine never races them.
		titleWG.Wait()

		// Handle commands
		if input == "/file" || strings.HasPrefix(input, "/file ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/file"))
			if path == "" {
				// No path given: browse and pick one.
				picked, perr := pickFile()
				if perr != nil {
					ErrorStyle.Fprintf(w, "Error: %v\n", perr)
					continue
				}
				if picked == "" {
					continue // cancelled
				}
				path = picked
			}
			att, err := ReadAttachment(path)
			if err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", err)
			} else {
				pendingAttachments = append(pendingAttachments, att)
				DimStyle.Fprintf(w, "Attached: %s (%s, %d bytes)\n", att.Filename, att.MimeType, len(att.Data))
			}
			continue
		}
		if input == "/files" || strings.HasPrefix(input, "/files ") {
			pendingAttachments = cleanAttachments(w, pendingAttachments)
			continue
		}
		if input == "/model" || strings.HasPrefix(input, "/model ") {
			models, ferr := FetchModels(ctx, p)
			if ferr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", ferr)
				continue
			}
			selected, serr := SelectModel(models)
			if serr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", serr)
				continue
			}
			if selected == "" {
				continue // cancelled (ESC)
			}
			p.SetModel(selected)
			sw.SetModel(selected)
			DimStyle.Fprintf(w, "Model switched to %s\n", selected)
			continue
		}
		if input == "/session" || strings.HasPrefix(input, "/session ") {
			id, perr := PickSession()
			if perr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", perr)
				continue
			}
			if id == "" {
				DimStyle.Fprintln(w, "No session selected.")
				continue
			}
			if id == sw.ID() {
				DimStyle.Fprintln(w, "Already in this session.")
				continue
			}
			newSW, sess, lerr := ResumeSession(id, p)
			if lerr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", lerr)
				continue
			}
			sw.Close()
			sw = newSW
			history = sess.Messages
			persisted = len(history)
			// Re-seed the budget from the resumed history and drop the previous
			// session's token figures, so /status reflects the new session right
			// away — before any turn happens.
			budget.reseed(history)
			if ur, ok := p.(interface{ ResetUsage() }); ok {
				ur.ResetUsage()
			}
			pendingAttachments = nil
			titled = true
			// Continue under the current provider; adopt the session's model
			// only when it belongs to the same provider type.
			if sess.Meta.Provider == p.Type() && sess.Meta.Model != "" {
				p.SetModel(sess.Meta.Model)
			}
			DimStyle.Fprintf(w, "Resumed session %s (%d messages)\n", id, len(history))
			continue
		}
		if input == "/sessions" || strings.HasPrefix(input, "/sessions ") {
			cleanSessions(w, sw.ID())
			continue
		}
		if input == "/context" || strings.HasPrefix(input, "/context ") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/context"))
			if arg == "" {
				selected, perr := pickContextWindow(budget)
				if perr != nil {
					ErrorStyle.Fprintf(w, "Error: %v\n", perr)
					continue
				}
				if selected > 0 {
					budget.setWindow(selected)
					DimStyle.Fprintf(w, "Context window: %s\n", budget.status())
				}
				continue
			}
			n, perr := ParseWindowSize(arg)
			if perr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", perr)
				continue
			}
			budget.setWindow(n)
			DimStyle.Fprintf(w, "Context window: %s\n", budget.status())
			continue
		}
		if input == "/compact" || strings.HasPrefix(input, "/compact ") {
			hint := strings.TrimSpace(strings.TrimPrefix(input, "/compact"))
			compactNow(hint, true)
			continue
		}
		if input == "/mcp" || strings.HasPrefix(input, "/mcp ") {
			printMCPStatus(mgr, w)
			continue
		}
		if input == "/tools" || strings.HasPrefix(input, "/tools ") {
			printToolStatus(dispatch, mgr, w)
			continue
		}
		if input == "/status" || strings.HasPrefix(input, "/status ") {
			showStatus(statusLines(p, budget, history, len(pendingAttachments), dispatch, mgr, sw))
			continue
		}

		// Lazy model selection: if startup selection was skipped (ESC), pick a
		// model before sending the first real message. Cancelling again skips
		// this turn and returns to the prompt.
		if p.Model() == "" && !ensureModel(ctx, p, sw, w) {
			continue
		}

		// Auto-compact if this turn's new content would push past the threshold.
		extra := budget.counter.count(input)
		for _, att := range pendingAttachments {
			extra += len(att.Data) / 1000
		}
		if budget.shouldCompact(extra) {
			compactNow("", false)
		}

		msg := provider.Message{Role: "user", Content: input, Attachments: pendingAttachments}
		pendingAttachments = nil
		history = append(history, msg)

		// Use tool-call loop if provider supports tools and MCP tools are available
		if isToolProvider && len(tools) > 0 {
			var reply, thinking string
			historyLen := len(history)
			retryErr := retryWithCountdown(w, func() error {
				history = history[:historyLen]
				var err error
				reply, thinking, err = executeWithTools(ctx, tp, dispatch, &history, tools, w, false)
				return err
			})
			if retryErr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n\n", retryErr)
				history = history[:historyLen-1]
				continue
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w)
			history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
			persistTurn()
			budget.update(p, history)
			maybeTitle()
			continue
		}

		// Standard streaming path (no tools)
		var reply, thinking string
		retryErr := retryWithCountdown(w, func() error {
			var err error
			reply, thinking, err = streamResponse(ctx, p, history, w)
			return err
		})
		if retryErr != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n\n", retryErr)
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w)
		history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
		persistTurn()
		budget.update(p, history)
		maybeTitle()
	}
}

// ensureModel makes sure a model is selected before sending. Used for lazy
// selection when the startup model picker was skipped with ESC. Returns false
// if the user cancels selection (the caller should skip the turn).
func ensureModel(ctx context.Context, p provider.Provider, sw *SessionWriter, w io.Writer) bool {
	if p.Model() != "" {
		return true
	}
	models, err := FetchModels(ctx, p)
	if err != nil {
		ErrorStyle.Fprintf(w, "Error: %v\n", err)
		return false
	}
	selected, err := SelectModel(models)
	if err != nil {
		ErrorStyle.Fprintf(w, "Error: %v\n", err)
		return false
	}
	if selected == "" {
		return false // cancelled
	}
	p.SetModel(selected)
	sw.SetModel(selected)
	DimStyle.Fprintf(w, "Using model: %s\n", selected)
	return true
}

// generateTitle asks the model for a short conversation title and stores it on
// the session. Best-effort: any error leaves the placeholder title in place.
func generateTitle(ctx context.Context, p provider.Provider, firstUser, firstAssistant string, sw *SessionWriter) {
	prompt := fmt.Sprintf("Write a short title (at most 6 words, no quotes, no trailing punctuation) for the conversation below, in the same language the conversation uses. Return only the title itself:\n\nUser: %s\n\nAssistant: %s",
		truncateRunes(firstUser, 500), truncateRunes(firstAssistant, 500))
	title, err := p.Chat(ctx, []provider.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return
	}
	title = sanitizeTitle(title)
	if title != "" {
		sw.SetTitle(title)
	}
}

func sanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, "\"'“”「」` ")
	return truncateRunes(s, 80)
}

// truncateRunes truncates on rune boundaries so CJK text is never cut mid-rune.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// streamResponse handles the standard streaming display (reasoning + content pipes).
// Returns (content, reasoning, error).
func streamResponse(ctx context.Context, p provider.Provider, history []provider.Message, w io.Writer) (string, string, error) {
	reasonPr, reasonPw := io.Pipe()
	contentPr, contentPw := io.Pipe()
	var reply, thinking string
	var streamErr error
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer contentPw.Close()
		reply, thinking, streamErr = p.StreamChat(ctx, history, contentPw, reasonPw)
	}()

	firstChunk := make([]byte, 4096)
	var firstN int
	var readErr error
	hasReasoning := false

	WithSpinner("Thinking...", func() {
		firstN, readErr = reasonPr.Read(firstChunk)
		if readErr != nil {
			readErr = nil
			firstN, readErr = contentPr.Read(firstChunk)
		} else {
			hasReasoning = true
		}
	})

	if readErr != nil {
		<-done
		if streamErr != nil {
			return "", "", streamErr
		}
		return "", "", readErr
	}

	if hasReasoning {
		DimStyle.Fprint(w, "Reasoning> ")
		os.Stdout.WriteString("\033[2m")
		os.Stdout.Write(firstChunk[:firstN])
		io.Copy(os.Stdout, reasonPr)
		os.Stdout.WriteString("\033[0m")
		fmt.Fprintln(w)

		firstN, readErr = contentPr.Read(firstChunk)
		if readErr != nil {
			<-done
			if streamErr != nil {
				return "", "", streamErr
			}
			// Reasoning-only response
			return thinking, thinking, nil
		}
	}

	AssistantStyle.Fprint(w, "Assistant> ")
	mdw := newMarkdownWriter(os.Stdout)
	mdw.Write(firstChunk[:firstN])
	io.Copy(mdw, contentPr)
	mdw.Flush()
	<-done

	if streamErr != nil {
		return "", "", streamErr
	}

	return reply, thinking, nil
}

// executeWithTools runs the tool-call loop: calls the model, executes any tool
// calls via MCP, feeds results back, and repeats until the model produces a
// final text response. When quiet=true, no spinner/prefixes/reasoning/markdown
// are rendered — only the final text reply is returned via the content value.
func executeWithTools(ctx context.Context, tp provider.ToolProvider, dispatch tool.Dispatcher, history *[]provider.Message, tools []provider.ToolDef, w io.Writer, quiet bool) (string, string, error) {
	// Persistent spinner across all tool-call rounds
	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Writer = os.Stderr
	spinnerRunning := false

	startSpinner := func(suffix string) {
		if quiet {
			return
		}
		s.Suffix = " " + suffix
		if !spinnerRunning {
			s.Start()
			spinnerRunning = true
		}
	}
	stopSpinner := func() {
		if spinnerRunning {
			s.Stop()
			spinnerRunning = false
		}
	}

	for {
		reasonPr, reasonPw := io.Pipe()
		contentPr, contentPw := io.Pipe()
		var content, reasoning string
		var toolCalls []provider.ToolCall
		var streamErr error
		done := make(chan struct{})

		go func() {
			defer close(done)
			defer contentPw.Close()
			content, reasoning, toolCalls, streamErr = tp.StreamChatWithTools(ctx, *history, tools, contentPw, reasonPw)
		}()

		firstChunk := make([]byte, 4096)
		var firstN int
		var readErr error
		hasReasoning := false

		startSpinner("Thinking...")
		firstN, readErr = reasonPr.Read(firstChunk)
		if readErr != nil {
			readErr = nil
			firstN, readErr = contentPr.Read(firstChunk)
		} else {
			hasReasoning = true
		}

		if readErr != nil {
			<-done
			stopSpinner()
			if streamErr != nil {
				return "", "", streamErr
			}
			// EOF on content pipe might mean tool calls with no text
			if len(toolCalls) > 0 {
				goto handleToolCalls
			}
			return "", "", readErr
		}

		stopSpinner()

		if hasReasoning {
			if quiet {
				io.Copy(io.Discard, reasonPr)
			} else {
				DimStyle.Fprint(w, "Reasoning> ")
				os.Stdout.WriteString("\033[2m")
				os.Stdout.Write(firstChunk[:firstN])
				io.Copy(os.Stdout, reasonPr)
				os.Stdout.WriteString("\033[0m")
				fmt.Fprintln(w)
			}

			firstN, readErr = contentPr.Read(firstChunk)
			if readErr != nil {
				<-done
				if streamErr != nil {
					return "", "", streamErr
				}
				if len(toolCalls) > 0 {
					goto handleToolCalls
				}
				// Reasoning-only response
				return reasoning, reasoning, nil
			}
		}

		// Stream content to display
		if firstN > 0 {
			if quiet {
				io.Copy(io.Discard, contentPr)
			} else {
				AssistantStyle.Fprint(w, "Assistant> ")
				mdw := newMarkdownWriter(os.Stdout)
				mdw.Write(firstChunk[:firstN])
				io.Copy(mdw, contentPr)
				mdw.Flush()
			}
		} else {
			io.Copy(io.Discard, contentPr)
		}
		<-done

		if streamErr != nil {
			return "", "", streamErr
		}

		if len(toolCalls) == 0 {
			return content, reasoning, nil
		}

	handleToolCalls:
		if content != "" && !quiet {
			fmt.Fprintln(w)
		}

		// Append assistant message with tool calls to history
		msg := provider.Message{
			Role:      "assistant",
			Content:   content,
			ToolCalls: toolCalls,
		}
		// Preserve raw model content (e.g. Vertex AI thought signatures)
		if rcp, ok := tp.(provider.RawContentProvider); ok {
			msg.RawContent = rcp.LastRawContent()
		}
		*history = append(*history, msg)

		// Execute each tool call: print a header when it starts, run it under a
		// spinner (elapsed time + ESC), then print a short result summary below.
		for _, tc := range toolCalls {
			if !quiet {
				stopSpinner() // drop any "Thinking…" frame before the header
				CodeStyle.Fprintln(w, toolCallHeader(tc))
			}

			startSpinner(tc.Name)
			resultText, isError, callErr := callTool(ctx, dispatch, tc, s, tc.Name, quiet)
			if callErr != nil {
				resultText = fmt.Sprintf("Error calling tool: %v", callErr)
				isError = true
			}

			if !quiet {
				stopSpinner()
				printToolResult(w, resultText, isError)
			}

			// Append tool result message
			*history = append(*history, provider.Message{
				Role:         "tool",
				Content:      resultText,
				ToolCallID:   tc.ID,
				ToolCallName: tc.Name,
				IsError:      isError,
			})
		}
		// Spinner restarts as "Thinking…" at the top of the next round.
	}

}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// toolCallHeader renders the one-line "[name key:val key:val]" header shown when
// a tool call starts. Keys are sorted for stable output; each value is collapsed
// to one line and truncated.
func toolCallHeader(tc provider.ToolCall) string {
	keys := make([]string, 0, len(tc.Arguments))
	for k := range tc.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, tc.Name)
	for _, k := range keys {
		v := strings.ReplaceAll(fmt.Sprintf("%v", tc.Arguments[k]), "\n", " ")
		parts = append(parts, k+":"+truncateRunes(v, 40))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// toolResultMaxLines is how many lines of a tool result are shown inline; extra
// lines collapse to a "… +N lines" tail.
const toolResultMaxLines = 3

// printToolResult prints a tool result as up to toolResultMaxLines indented lines
// under the call, led by a "⎿" marker; further lines collapse to "… +N lines".
// Error results are red, otherwise faint.
func printToolResult(w io.Writer, result string, isError bool) {
	style := DimStyle
	if isError {
		style = ErrorStyle
	}

	result = strings.TrimRight(result, "\n")
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	lines := strings.Split(result, "\n")

	show, extra := lines, 0
	if len(lines) > toolResultMaxLines {
		show = lines[:toolResultMaxLines-1] // leave the last row for the tail
		extra = len(lines) - len(show)
	}
	for i, ln := range show {
		ln = truncateRunes(ln, 120)
		if i == 0 {
			style.Fprintf(w, "  ⎿ %s\n", ln)
		} else {
			style.Fprintf(w, "    %s\n", ln)
		}
	}
	if extra > 0 {
		style.Fprintf(w, "    … +%d lines\n", extra)
	}
}

// toolRow is one row of the /tools panel; its fields are styled independently by
// the panel's templates.
type toolRow struct {
	Name   string // tool name
	Source string // "built-in" or "mcp: <server>"
	Desc   string // one-line description
	IsMCP  bool   // true when Source is an MCP server
}

// /tools panel templates (promptui FuncMap). Columns are styled independently:
// bold name, the source tag green ([built-in]) or yellow ([mcp: …]), faint
// description; the cyan "▸" marks the active (scroll) row.
const (
	toolPanelSrcTag   = `{{ if .IsMCP }}{{ printf "[%s]" .Source | yellow }}{{ else }}{{ "[built-in]" | green }}{{ end }}`
	toolPanelActive   = `{{ "▸" | cyan }} {{ printf "%-18s" .Name | bold }}  ` + toolPanelSrcTag + `  {{ .Desc | faint }}`
	toolPanelInactive = `  {{ printf "%-18s" .Name | bold }}  ` + toolPanelSrcTag + `  {{ .Desc | faint }}`
)

// printToolStatus lists every tool currently advertised to the model, with its
// source (a built-in tool or which MCP server it came from) and a one-line
// description — the tool-level counterpart to /mcp's server-level view. The list
// is often long, so it is shown in a scrollable, read-only promptui panel: arrow
// keys scroll, and any selection / Enter / Esc dismisses it (no cancel row needed
// since the choice is irrelevant).
func printToolStatus(dispatch tool.Dispatcher, mgr *mcpmgr.Manager, w io.Writer) {
	var defs []provider.ToolDef
	if dispatch != nil {
		defs = dispatch.Tools()
	}
	if len(defs) == 0 {
		DimStyle.Fprintln(w, "No tools available.")
		return
	}

	// Map each MCP tool name to its server for source attribution; anything not
	// from an MCP server is a built-in.
	source := make(map[string]string)
	if mgr != nil {
		for _, s := range mgr.Servers() {
			for _, name := range s.Tools {
				source[name] = s.Name
			}
		}
	}

	rows := make([]toolRow, len(defs))
	for i, d := range defs {
		r := toolRow{
			Name:   d.Name,
			Source: "built-in",
			Desc:   truncate(strings.ReplaceAll(d.Description, "\n", " "), 80),
		}
		if srv, ok := source[d.Name]; ok {
			r.Source = "mcp: " + srv
			r.IsMCP = true
		}
		rows[i] = r
	}

	// Read-only viewer: a scrollable promptui Select. ESC/q close it natively;
	// HideSelected wipes the panel on exit. Columns are styled independently —
	// bold name, the source tag green ([built-in]) or yellow ([mcp: …]), and a
	// faint description. The cyan "▸" marks the row the arrows scroll.
	size := len(rows)
	if size > 15 {
		size = 15
	}
	prompt := promptui.Select{
		Label:        fmt.Sprintf("Tools (%d)", len(defs)),
		Items:        rows,
		Size:         size,
		HideHelp:     true,
		HideSelected: true,
		Templates: &promptui.SelectTemplates{
			Label:    `{{ . | bold }}  {{ "Enter/Esc to close" | faint }}`,
			Active:   toolPanelActive,
			Inactive: toolPanelInactive,
		},
	}
	_, _, _ = prompt.Run()
}

func printMCPStatus(mgr *mcpmgr.Manager, w io.Writer) {
	servers := mgr.Servers()
	if len(servers) == 0 {
		DimStyle.Fprintln(w, "No MCP servers configured.")
		return
	}

	totalTools := 0
	for _, s := range servers {
		totalTools += s.ToolCount
	}
	fmt.Fprintf(w, "MCP servers: %d, total tools: %d\n", len(servers), totalTools)
	fmt.Fprintln(w)

	for _, s := range servers {
		status := ErrorStyle.Sprint("disconnected")
		if s.Connected {
			status = BoldStyle.Sprint("connected")
		}
		fmt.Fprintf(w, "  %s [%s]\n", BoldStyle.Sprint(s.Name), status)
		DimStyle.Fprintf(w, "    Endpoint: %s\n", s.Endpoint)
		if s.ToolCount == 0 {
			DimStyle.Fprintln(w, "    Tools: (none)")
		} else {
			DimStyle.Fprintf(w, "    Tools (%d): %s\n", s.ToolCount, strings.Join(s.Tools, ", "))
		}
	}
}
