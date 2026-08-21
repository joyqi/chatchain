package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"chatchain/internal/agents"
	"chatchain/internal/llm"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"
)

func FetchModels(ctx context.Context, p provider.Provider) ([]string, error) {
	DimStyle.Fprintln(os.Stderr, "Fetching available models...")
	return p.ListModels(ctx)
}

// Once is the -m path: one send, no REPL, no session. format selects what
// lands on w — the reply alone, or the whole run as a result object.
//
// The two formats differ in what an ERROR means, and that is the reason for
// the split below. In text mode a failure prints nothing and travels out as
// the return value. In JSON mode the report IS the output whether or not the
// run succeeded — the rounds that completed were billed, and suppressing them
// would leave exactly the runs worth investigating with no numbers. The error
// still travels either way, so the exit status keeps meaning what it did.
func Once(ctx context.Context, p provider.Provider, message string, systemPrompt string, dispatch tool.Dispatcher, agent AgentOptions, maxTurns int, format OutputFormat, w io.Writer) error {
	rec := newRunRecorder()
	reply, images, imageErrs, err := runOnce(ctx, p, message, systemPrompt, dispatch, agent, maxTurns, rec)

	if format == OutputJSON {
		if werr := writeReport(w, rec.report(p, reply, images, imageErrs, err)); werr != nil {
			return werr
		}
		return err
	}
	if err != nil {
		return err
	}
	if reply != "" {
		fmt.Fprintln(w, reply)
	}
	for _, path := range images {
		fmt.Fprintf(w, "🖼 saved: %s\n", path)
	}
	for _, msg := range imageErrs {
		fmt.Fprintln(w, msg)
	}
	return nil
}

// runOnce performs the send and reports what came back, writing nothing. Once
// owns the formatting; keeping this half output-free is what lets a failed
// run still be described.
func runOnce(ctx context.Context, p provider.Provider, message string, systemPrompt string, dispatch tool.Dispatcher, agent AgentOptions, maxTurns int, rec *runRecorder) (reply string, images, imageErrs []string, err error) {
	var messages []provider.Message
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, provider.Message{Role: "user", Content: message})

	// Agent mode composes the AGENTS.md/skills overlay once for the single
	// send — the same volatile-overlay semantics as the REPL, minus the
	// per-turn freshness loop.
	var sendOverlay string
	if agent.Enabled {
		cwd, cerr := os.Getwd()
		if cerr != nil || cwd == "" {
			cwd = agent.Root
		}
		ov := agents.NewOverlay(agent.Root, cwd)
		ov.Refresh()
		sendOverlay = ov.Content()
	}

	tp, isToolProvider := p.(provider.ToolProvider)
	if h, ok := p.(interface {
		SetToolSearcher(func(string) []provider.ToolDef)
	}); ok && dispatch != nil {
		if ts, ok2 := dispatch.(tool.ToolSearcher); ok2 {
			h.SetToolSearcher(ts.SearchTools)
		}
	}
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}

	if isToolProvider && len(tools) > 0 {
		reply, _, err = executeWithTools(ctx, tp, dispatch, &messages, tools, sendOverlay, maxTurns, rec)
		if err != nil {
			return "", nil, nil, err
		}
		images, imageErrs = saveImagesQuiet(tp)
		return reply, images, imageErrs, nil
	}

	reply, err = p.Chat(ctx, agents.ComposeSendHistory(messages, sendOverlay))
	if err != nil {
		return "", nil, nil, err
	}
	// The tool loop records each of its rounds; this path has exactly one.
	rec.observe(p, nil)
	images, imageErrs = saveImagesQuiet(p)
	return reply, images, imageErrs, nil
}

const maxRetries = 10

// http4xxPattern matches HTTP 4xx status codes (except 429) in error messages.
var http4xxPattern = regexp.MustCompile(`\b4\d{2}\b`)

// isRetryable returns true if the error is likely transient and worth retrying.
// Non-retryable: io.EOF, user interruption, the tool-loop cap, a non-streaming
// stream response, HTTP 4xx (except 429 rate limit). Errors from internal/llm
// carry a structured status; anything else falls back to the historical
// string scan (SDK-shaped errors until every provider is ported).
func isRetryable(err error) bool {
	if err == io.EOF || errors.Is(err, errInterrupted) || errors.Is(err, errToolRoundsExceeded) || errors.Is(err, llm.ErrNoEvents) {
		return false
	}
	// Provider-declared deterministic failures (safety filters, malformed
	// turns): a retry would repeat the same billed call for the same result.
	var pe *provider.PermanentError
	if errors.As(err, &pe) {
		return false
	}
	var se *llm.StatusError
	if errors.As(err, &se) {
		return se.Status == 429 || se.Status >= 500
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

const userPrompt = "❯ "

// firstUserText is what a session is named after: the first user message
// with any text. The assistant reply is deliberately NOT an input — the name
// must not wait for it (a tool-heavy first turn takes minutes), so neither
// the placeholder nor the model pass may depend on it.
func firstUserText(history []provider.Message) string {
	for _, m := range history {
		if m.Role == "user" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

func generateTitleText(ctx context.Context, p provider.Provider, firstUser string) string {
	prompt := fmt.Sprintf("Write a short title (at most 6 words, no quotes, no trailing punctuation) summarizing the user message below, in the same language the message uses. Return only the title itself:\n\n%s",
		truncateRunes(firstUser, 500))
	title, err := p.Chat(ctx, []provider.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return ""
	}
	return sanitizeTitle(title)
}

// isReadOnlyViewer reports whether input is a command that only opens a
// read-only viewer — it never calls the provider or mutates the session writer,
// so it need not wait on background title generation.
func isReadOnlyViewer(input string) bool {
	for _, c := range []string{"/debug", "/status", "/tools", "/skills"} {
		if input == c || strings.HasPrefix(input, c+" ") {
			return true
		}
	}
	return false
}

func sanitizeTitle(s string) string {
	s = strings.TrimSpace(stripThink(s))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] // a model that explains itself: keep the title line only
	}
	s = strings.Trim(s, "\"'“”「」` ")
	return titleFrom(s, 80)
}

// stripThink removes <think>…</think> blocks — reasoning models behind
// chatcomp relays leak them into plain content, and a chain of thought must
// never become the session title. An unclosed tag means everything after it
// is thought.
func stripThink(s string) string {
	for {
		open := strings.Index(s, "<think>")
		if open < 0 {
			return s
		}
		end := strings.Index(s[open:], "</think>")
		if end < 0 {
			return s[:open]
		}
		s = s[:open] + s[open+end+len("</think>"):]
	}
}

// notifyDigest shapes a completed reply into the attention ping's text — the
// first content line with its markdown dressing stripped, capped for a
// notification banner. A digest beats a fixed phrase: the user deciding
// whether to switch back deserves a peek at the answer (Claude Code does the
// same). The fallback covers empty replies.
func notifyDigest(reply string) string {
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimLeft(line, "#>-*+ \t")
		line = strings.NewReplacer("**", "", "`", "").Replace(line)
		if line = strings.TrimSpace(line); line != "" {
			return titleFrom(line, 60)
		}
	}
	return "Response ready"
}

// titleFrom shapes arbitrary text into a session title: ONE line (a stored
// newline would break the session picker's row accounting and the window
// title), control characters dropped, capped on rune boundaries. Every title
// entry point funnels through it — the LLM's answer, the prompt-derived
// placeholder image providers rely on, and an explicit /save argument.
func titleFrom(s string, max int) string {
	return truncateRunes(flattenLine(s), max)
}

// truncateRunes truncates on rune boundaries so CJK text is never cut mid-rune.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// appTitle is the terminal-title fallback before a session has a real title.
const appTitle = "chatchain"

// errToolRoundsExceeded is returned when an explicit --max-turns limit is
// hit. Non-retryable (see isRetryable): retrying a runaway loop would only
// run it again. There is NO default cap — interactively the user is the brake
// (ESC + tool approval gates), matching Claude Code/Codex/Cursor/Cline; for
// the non-interactive path the -m caller opts in via --max-turns.
var errToolRoundsExceeded = errors.New("tool loop reached the --max-turns limit without a final response")

// executeWithTools runs the non-interactive (Once) tool loop: each round
// streams one StreamChatWithTools call quietly (nothing renders — the final
// text is returned whole), executes any tool calls, feeds the results back,
// and repeats until the model answers without tools (or an explicit maxTurns
// limit trips; 0 = unlimited). The interactive twin with rendering and
// interrupts is toolLoop.
//
// overlay is the turn's agent-mode system overlay: each round sends a
// send-time copy of *history with it applied (see agents.ComposeSendHistory), while
// the appended assistant/tool messages land in the clean *history. Empty
// means none — every round then sends *history itself, exactly as before.
//
// rec observes each completed round for the run report. It is filled whatever
// the output format, because the cost of a few ints per round is not worth a
// conditional, and because a run that fails mid-loop still owes an account of
// the rounds it did pay for.
func executeWithTools(ctx context.Context, tp provider.ToolProvider, dispatch tool.Dispatcher, history *[]provider.Message, tools []provider.ToolDef, overlay string, maxTurns int, rec *runRecorder) (string, string, error) {
	for rounds := 0; ; rounds++ {
		if maxTurns > 0 && rounds == maxTurns {
			return "", "", fmt.Errorf("%w (%d turns)", errToolRoundsExceeded, maxTurns)
		}
		if dispatch != nil && rounds > 0 {
			// The advertised set is LIVE: tools a search_tools round loaded
			// (and late-connecting MCP servers) must appear the very next
			// request, not next turn.
			tools = dispatch.Tools()
		}
		if pl, ok := dispatch.(tool.PendingLoader); ok && dispatch != nil {
			// Frozen-mount defer (system-tools): loaded schemas append to
			// history as a system message carrying Tools.
			if defs := pl.TakePendingLoads(); len(defs) > 0 {
				*history = append(*history, provider.Message{Role: "system", Tools: defs})
			}
		}
		content, reasoning, toolCalls, err := tp.StreamChatWithTools(ctx,
			agents.ComposeSendHistory(*history, overlay), tools, io.Discard, nopWriteCloser{io.Discard})
		if err != nil {
			return "", "", err
		}
		// Read the accounting NOW: LastUsageFull reports the provider's most
		// recent call, so anything between here and the next round would
		// silently reassign this round's cost.
		rec.observe(tp, toolNames(toolCalls))
		if len(toolCalls) == 0 {
			if content == "" && reasoning != "" {
				// Reasoning-only response: the reasoning IS the answer.
				return reasoning, reasoning, nil
			}
			return content, reasoning, nil
		}

		msg := provider.Message{Role: "assistant", Content: content, ToolCalls: toolCalls}
		// Preserve raw model content (e.g. Vertex AI thought signatures).
		if rcp, ok := tp.(provider.RawContentProvider); ok {
			msg.RawContent = rcp.LastRawContent()
		}
		*history = append(*history, msg)

		for _, tc := range toolCalls {
			// Approval-requiring tools cannot ask anyone here: reject the call
			// with a result that tells the model (and the user reading the
			// transcript) how to enable it.
			if needsApproval(dispatch, tc.Name) {
				*history = append(*history, provider.Message{
					Role: "tool",
					Content: fmt.Sprintf("%s was not executed: it requires interactive approval, "+
						"which is unavailable in this non-interactive run. Set the toolset's auto-approve option "+
						"(tools.code.auto_write / tools.shell.auto_run) to permit it here.", tc.Name),
					ToolCallID:   tc.ID,
					ToolCallName: tc.Name,
					IsError:      true,
				})
				continue
			}
			resultText, isError, callErr := dispatch.CallTool(ctx, tc.Name, tc.Arguments)
			if callErr != nil {
				resultText = fmt.Sprintf("Error calling tool: %v", callErr)
				isError = true
			}
			*history = append(*history, provider.Message{
				Role:         "tool",
				Content:      resultText,
				ToolCallID:   tc.ID,
				ToolCallName: tc.Name,
				IsError:      isError,
			})
		}
	}
}

// needsApproval reports whether the dispatcher marks the named tool's calls
// as requiring interactive user approval (the tool.ApprovalReporter
// capability; parts without it — e.g. MCP servers — never require approval).
func needsApproval(dispatch tool.Dispatcher, name string) bool {
	ar, ok := dispatch.(tool.ApprovalReporter)
	return ok && ar.RequiresApproval(name)
}

// presentationOf reports the named tool's display class (the optional
// tool.PresentationReporter capability; dispatchers without it present
// everything grouped).
func presentationOf(dispatch tool.Dispatcher, name string) tool.Presentation {
	pr, ok := dispatch.(tool.PresentationReporter)
	if !ok {
		return tool.PresentGroup
	}
	return pr.Presentation(name)
}

// headerSummaryOf asks the dispatcher for the named tool's own call summary
// (the optional tool.HeaderReporter capability). ok=false — no capability, or
// a tool that declares none — means the generic argument digest applies.
func headerSummaryOf(dispatch tool.Dispatcher, name string, args map[string]any) (string, bool) {
	hr, ok := dispatch.(tool.HeaderReporter)
	if !ok {
		return "", false
	}
	return hr.HeaderSummary(name, args)
}

// supportsParallel reports whether THIS call may run concurrently with the
// round's others (the optional tool.ParallelReporter capability; dispatchers
// without it serialize everything, which is the safe answer). The arguments
// travel because the answer can differ between two calls to one tool — see
// tool.parallelizer.
func supportsParallel(dispatch tool.Dispatcher, call provider.ToolCall) bool {
	pr, ok := dispatch.(tool.ParallelReporter)
	return ok && pr.SupportsParallel(call.Name, call.Arguments)
}

// isInteractive reports whether the named tool runs its own user surface:
// its calls route to the attention channels instead of the activity panel.
func isInteractive(dispatch tool.Dispatcher, name string) bool {
	return presentationOf(dispatch, name) == tool.PresentSurface
}

// nopWriteCloser adapts a plain writer to the reasoning stream's WriteCloser.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

const toolHeaderMaxArgs = 3

// toolHeaderMaxValue is the display width an argument value is truncated to (with
// a trailing ellipsis) so a single long value can't blow up the header.
const toolHeaderMaxValue = 15

// toolCallHeader renders the one-line "[name key:val key:val]" header shown when
// a tool call starts. Keys are sorted for stable output; each value is collapsed
// to one line and truncated, and arguments past toolHeaderMaxArgs collapse to a
// "… +N args" tail.
func toolCallHeader(dispatch tool.Dispatcher, tc provider.ToolCall) string {
	// A tool that writes its own summary takes over completely — an empty
	// one renders as a bare "[name]", never as the argument digest below
	// (see tool.headliner: edit_file's new_string must not reach a header).
	if summary, ok := headerSummaryOf(dispatch, tc.Name, tc.Arguments); ok {
		name := displayToolName(tc.Name)
		if summary == "" {
			return "[" + name + "]"
		}
		return "[" + name + " " + summary + "]"
	}
	keys := make([]string, 0, len(tc.Arguments))
	for k := range tc.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	shown := keys
	if len(shown) > toolHeaderMaxArgs {
		shown = shown[:toolHeaderMaxArgs]
	}

	parts := make([]string, 0, len(shown)+2)
	parts = append(parts, displayToolName(tc.Name))
	for _, k := range shown {
		v := strings.ReplaceAll(fmt.Sprintf("%v", tc.Arguments[k]), "\n", " ")
		parts = append(parts, k+":"+truncateRunes(v, toolHeaderMaxValue))
	}
	if extra := len(keys) - len(shown); extra > 0 {
		parts = append(parts, fmt.Sprintf("… +%d args", extra))
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

// toolStatusLines lists every tool the model can reach — advertised ones with
// their source (built-in or MCP server), plus deferred-but-hidden ones dimmed
// at the bottom with their load state. Rendered in the /tools "Tools" tab.
func toolStatusLines(dispatch tool.Dispatcher, mgr *mcpmgr.Manager) []string {
	var defs []provider.ToolDef
	if dispatch != nil {
		defs = dispatch.Tools()
	}

	// Deferred state: an advertised tool that came in via search keeps a
	// "loaded" badge; still-hidden ones list after the advertised set.
	deferState := map[string]tool.DeferredToolStatus{}
	var hidden []tool.DeferredToolStatus
	if di, ok := dispatch.(tool.DeferInspector); ok {
		advertised := make(map[string]bool, len(defs))
		for _, d := range defs {
			advertised[d.Name] = true
		}
		for _, st := range di.DeferredTools() {
			deferState[st.Name] = st
			if !advertised[st.Name] {
				hidden = append(hidden, st)
			}
		}
		sort.Slice(hidden, func(i, j int) bool {
			if hidden[i].Group != hidden[j].Group {
				return hidden[i].Group < hidden[j].Group
			}
			return hidden[i].Name < hidden[j].Name
		})
	}
	if len(defs) == 0 && len(hidden) == 0 {
		return []string{DimStyle.Sprint("No tools available.")}
	}

	// Map each MCP tool's wire name to its server (ServerStatus.Tools holds raw
	// names, so recompose the wire name from the manager-assigned segment,
	// which is what registration used); anything else is a built-in. The tag
	// keeps the original server name — the segment is a wire detail.
	source := make(map[string]string)
	if mgr != nil {
		for _, s := range mgr.Servers() {
			for _, name := range s.Tools {
				source[mcpmgr.ComposeWireName(s.Segment, name)] = s.Name
			}
		}
	}

	// Pad names to the longest one so the source tags line up; namespaced
	// display names ("server:tool") routinely exceed any fixed width.
	names := make([]string, len(defs))
	hiddenNames := make([]string, len(hidden))
	width := 0
	for i, d := range defs {
		names[i] = displayToolName(d.Name)
		if len(names[i]) > width {
			width = len(names[i])
		}
	}
	for i, st := range hidden {
		hiddenNames[i] = displayToolName(st.Name)
		if len(hiddenNames[i]) > width {
			width = len(hiddenNames[i])
		}
	}

	head := DimStyle.Sprintf("%d tool(s) available", len(defs))
	if len(deferState) > 0 {
		head += DimStyle.Sprintf(" · %d deferred (%d loaded)", len(deferState), len(deferState)-len(hidden))
	}
	lines := make([]string, 0, len(defs)+len(hidden)+1)
	lines = append(lines, head)
	for i, d := range defs {
		tag := CodeBlockStyle.Sprint("[built-in]") // green
		if srv, ok := source[d.Name]; ok {
			label := srv
			if st, ok := deferState[d.Name]; ok {
				label += " · " + st.State
			}
			tag = YellowStyle.Sprintf("[mcp: %s]", label)
		}
		desc := strings.ReplaceAll(d.Description, "\n", " ")
		lines = append(lines, fmt.Sprintf("%s  %s  %s", BoldStyle.Sprintf("%-*s", width, names[i]), tag, DimStyle.Sprint(desc)))
	}
	for i, st := range hidden {
		desc := strings.ReplaceAll(st.Description, "\n", " ")
		lines = append(lines, fmt.Sprintf("%s  %s  %s",
			DimStyle.Sprintf("%-*s", width, hiddenNames[i]),
			DimStyle.Sprintf("[mcp: %s · %s]", st.Group, st.State),
			DimStyle.Sprint(desc)))
	}
	return lines
}

// mcpStatusLines describes every configured MCP server — connection state,
// endpoint, tools, and any error — as display lines prefixed by a one-line
// summary. Rendered in the /tools "MCP" tab.
func mcpStatusLines(mgr *mcpmgr.Manager, dispatch tool.Dispatcher) []string {
	// Per-server defer tallies for the status row (state "loaded" counts
	// against the group's total).
	type tally struct{ total, loaded int }
	deferBy := map[string]*tally{}
	if di, ok := dispatch.(tool.DeferInspector); ok {
		for _, st := range di.DeferredTools() {
			t := deferBy[st.Group]
			if t == nil {
				t = &tally{}
				deferBy[st.Group] = t
			}
			t.total++
			if st.State == "loaded" {
				t.loaded++
			}
		}
	}
	if mgr == nil {
		return []string{DimStyle.Sprint("No MCP servers configured.")}
	}
	servers := mgr.Servers()
	if len(servers) == 0 {
		return []string{DimStyle.Sprint("No MCP servers configured.")}
	}

	totalTools := 0
	for _, s := range servers {
		totalTools += s.ToolCount
	}

	lines := []string{DimStyle.Sprintf("%d server(s) · %d tool(s)", len(servers), totalTools)}
	for _, s := range servers {
		status := ErrorStyle.Sprint("disconnected")
		switch {
		case s.Connected:
			status = CodeBlockStyle.Sprint("connected") // green
		case s.Pending:
			status = DimStyle.Sprint("connecting…")
		}
		if t, ok := deferBy[s.Name]; ok && t.total > 0 {
			status += DimStyle.Sprintf(" · deferred (%d/%d loaded)", t.loaded, t.total)
		}
		lines = append(lines, fmt.Sprintf("%s  [%s]", BoldStyle.Sprint(s.Name), status))
		lines = append(lines, DimStyle.Sprintf("  endpoint: %s", s.Endpoint))
		if s.ToolCount == 0 {
			lines = append(lines, DimStyle.Sprint("  tools: (none)"))
		} else {
			lines = append(lines, DimStyle.Sprintf("  tools (%d): %s", s.ToolCount, strings.Join(s.Tools, ", ")))
		}
		if !s.Connected && s.Err != "" {
			lines = append(lines, ErrorStyle.Sprintf("  error: %s", strings.SplitN(s.Err, "\n", 2)[0]))
		}
	}
	return lines
}
