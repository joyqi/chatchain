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

	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"
)

func FetchModels(ctx context.Context, p provider.Provider) ([]string, error) {
	DimStyle.Fprintln(os.Stderr, "Fetching available models...")
	return p.ListModels(ctx)
}

func Once(ctx context.Context, p provider.Provider, message string, systemPrompt string, dispatch tool.Dispatcher, agent AgentOptions, w io.Writer) error {
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
		ov := newSystemOverlay(agent.Root, cwd)
		ov.refresh()
		sendOverlay = ov.content()
	}

	tp, isToolProvider := p.(provider.ToolProvider)
	var tools []provider.ToolDef
	if dispatch != nil {
		tools = dispatch.Tools()
	}

	if isToolProvider && len(tools) > 0 {
		reply, _, err := executeWithTools(ctx, tp, dispatch, &messages, tools, sendOverlay)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, reply)
		return nil
	}

	reply, err := p.Chat(ctx, composeSendHistory(messages, sendOverlay))
	if err != nil {
		return err
	}
	fmt.Fprintln(w, reply)
	return nil
}

const maxRetries = 10

// http4xxPattern matches HTTP 4xx status codes (except 429) in error messages.
var http4xxPattern = regexp.MustCompile(`\b4\d{2}\b`)

// isRetryable returns true if the error is likely transient and worth retrying.
// Non-retryable: io.EOF, user interruption, the tool-loop cap, HTTP 4xx
// (except 429 rate limit).
func isRetryable(err error) bool {
	if err == io.EOF || errors.Is(err, errInterrupted) || errors.Is(err, errToolRoundsExceeded) {
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

const userPrompt = "❯ "

func generateTitleText(ctx context.Context, p provider.Provider, firstUser, firstAssistant string, sw *SessionWriter) string {
	prompt := fmt.Sprintf("Write a short title (at most 6 words, no quotes, no trailing punctuation) for the conversation below, in the same language the conversation uses. Return only the title itself:\n\nUser: %s\n\nAssistant: %s",
		truncateRunes(firstUser, 500), truncateRunes(firstAssistant, 500))
	title, err := p.Chat(ctx, []provider.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return ""
	}
	title = sanitizeTitle(title)
	if title != "" {
		sw.SetTitle(title)
	}
	return title
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

// appTitle is the terminal-title fallback before a session has a real title.
const appTitle = "chatchain"

const maxToolRounds = 50

// errToolRoundsExceeded is returned when the loop cap is hit. Non-retryable
// (see isRetryable): retrying a runaway loop would only run it again.
var errToolRoundsExceeded = fmt.Errorf("tool loop exceeded %d rounds without a final response", maxToolRounds)

// executeWithTools runs the non-interactive (Once) tool loop: each round
// streams one StreamChatWithTools call quietly (nothing renders — the final
// text is returned whole), executes any tool calls, feeds the results back,
// and repeats until the model answers without tools or the maxToolRounds cap
// trips. The interactive twin with rendering and interrupts is toolLoop.
//
// overlay is the turn's agent-mode system overlay: each round sends a
// send-time copy of *history with it applied (see composeSendHistory), while
// the appended assistant/tool messages land in the clean *history. Empty
// means none — every round then sends *history itself, exactly as before.
func executeWithTools(ctx context.Context, tp provider.ToolProvider, dispatch tool.Dispatcher, history *[]provider.Message, tools []provider.ToolDef, overlay string) (string, string, error) {
	for rounds := 0; ; rounds++ {
		if rounds == maxToolRounds {
			return "", "", errToolRoundsExceeded
		}
		content, reasoning, toolCalls, err := tp.StreamChatWithTools(ctx,
			composeSendHistory(*history, overlay), tools, io.Discard, nopWriteCloser{io.Discard})
		if err != nil {
			return "", "", err
		}
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
func toolCallHeader(tc provider.ToolCall) string {
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

// toolStatusLines lists every tool advertised to the model — its source (a
// built-in tool or which MCP server it came from) and a one-line description — as
// display lines prefixed by a one-line summary. Rendered in the /tools "Tools"
// tab.
func toolStatusLines(dispatch tool.Dispatcher, mgr *mcpmgr.Manager) []string {
	var defs []provider.ToolDef
	if dispatch != nil {
		defs = dispatch.Tools()
	}
	if len(defs) == 0 {
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
	width := 0
	for i, d := range defs {
		names[i] = displayToolName(d.Name)
		if len(names[i]) > width {
			width = len(names[i])
		}
	}

	lines := make([]string, 0, len(defs)+1)
	lines = append(lines, DimStyle.Sprintf("%d tool(s) available", len(defs)))
	for i, d := range defs {
		tag := CodeBlockStyle.Sprint("[built-in]") // green
		if srv, ok := source[d.Name]; ok {
			tag = YellowStyle.Sprintf("[mcp: %s]", srv)
		}
		desc := strings.ReplaceAll(d.Description, "\n", " ")
		lines = append(lines, fmt.Sprintf("%s  %s  %s", BoldStyle.Sprintf("%-*s", width, names[i]), tag, DimStyle.Sprint(desc)))
	}
	return lines
}

// mcpStatusLines describes every configured MCP server — connection state,
// endpoint, tools, and any error — as display lines prefixed by a one-line
// summary. Rendered in the /tools "MCP" tab.
func mcpStatusLines(mgr *mcpmgr.Manager) []string {
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
