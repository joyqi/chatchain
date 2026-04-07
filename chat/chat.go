package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpmgr "chatchain/mcp"
	"chatchain/provider"

	"github.com/briandowns/spinner"
	"github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"golang.org/x/term"
)

func SelectModel(models []string) (string, error) {
	prompt := promptui.Select{
		Label: "Select a model",
		Items: models,
		Size:  15,
	}

	_, result, err := prompt.Run()
	if err != nil {
		return "", err
	}
	return result, nil
}

func withSpinner(title string, action func()) {
	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Suffix = " " + title
	s.Start()
	action()
	s.Stop()
}

func FetchModels(ctx context.Context, p provider.Provider) ([]string, error) {
	var models []string
	var fetchErr error

	withSpinner("Fetching available models...", func() {
		models, fetchErr = p.ListModels(ctx)
	})

	return models, fetchErr
}

func Once(ctx context.Context, p provider.Provider, message string, systemPrompt string, mgr *mcpmgr.Manager, w io.Writer) error {
	var messages []provider.Message
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, provider.Message{Role: "user", Content: message})

	tp, isToolProvider := p.(provider.ToolProvider)
	tools := mgr.Tools()

	if isToolProvider && len(tools) > 0 {
		reply, _, err := executeWithTools(ctx, tp, mgr, &messages, tools, w)
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

func ReadSystemPrompt(w io.Writer) (string, []provider.Message, error) {
	pf := &pasteFilter{r: os.Stdin}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          BoldStyle.Sprint("System> "),
		InterruptPrompt: "^C",
		AutoComplete:    &importCompleter{},
		Stdin:           pf,
	})
	if err != nil {
		return "", nil, err
	}
	defer rl.Close()

	os.Stdout.WriteString("\033[?2004h")
	defer os.Stdout.WriteString("\033[?2004l")

	for {
		input, err := rl.Readline()
		if err != nil {
			return "", nil, nil // skip on Ctrl+C / EOF
		}
		input = expandPasteTags(strings.TrimSpace(input), pf)

		if input == "/import" || strings.HasPrefix(input, "/import ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/import"))
			if path == "" {
				path = "history.md"
			}
			imported, err := ImportHistory(path)
			if err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", err)
				continue
			}
			DimStyle.Fprintf(w, "Imported %d messages from %s\n", len(imported), path)
			return "", imported, nil
		}

		return input, nil, nil
	}
}

type importCompleter struct{}

func (c *importCompleter) Do(line []rune, pos int) ([][]rune, int) {
	text := string(line[:pos])
	if !strings.HasPrefix(text, "/") {
		return nil, 0
	}
	if !strings.Contains(text, " ") {
		cmd := "/import "
		if strings.HasPrefix(cmd, text) {
			return [][]rune{[]rune(cmd[len(text):])}, len([]rune(text))
		}
		return nil, 0
	}
	if strings.HasPrefix(text, "/import ") {
		return completeFilePath(text[8:])
	}
	return nil, 0
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
		commands := []string{"/file ", "/files ", "/clear ", "/save ", "/import ", "/mcp "}
		var candidates [][]rune
		for _, cmd := range commands {
			if strings.HasPrefix(cmd, text) {
				candidates = append(candidates, []rune(cmd[len(text):]))
			}
		}
		return candidates, len([]rune(text))
	}

	// File path completion for "/file " and "/save "
	if strings.HasPrefix(text, "/file ") && !strings.HasPrefix(text, "/files") {
		return completeFilePath(text[6:])
	}
	if strings.HasPrefix(text, "/save ") {
		return completeFilePath(text[6:])
	}
	if strings.HasPrefix(text, "/import ") {
		return completeFilePath(text[8:])
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

func Run(p provider.Provider, systemPrompt string, importedHistory []provider.Message, mgr *mcpmgr.Manager, w io.Writer) error {
	pf := &pasteFilter{r: os.Stdin}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          UserStyle.Sprint("You> "),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		AutoComplete:    &chatCompleter{},
		Stdin:           pf,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize input: %w", err)
	}
	defer rl.Close()

	// Enable bracketed paste mode AFTER readline init, so readline's
	// terminal setup doesn't override it.
	os.Stdout.WriteString("\033[?2004h")
	defer os.Stdout.WriteString("\033[?2004l")

	var history []provider.Message
	if len(importedHistory) > 0 {
		history = importedHistory
	} else if systemPrompt != "" {
		history = append(history, provider.Message{Role: "system", Content: systemPrompt})
	}
	ctx := context.Background()

	DimStyle.Fprintln(w, "Chat started. Press Ctrl+C to exit.")
	DimStyle.Fprintln(w, "Commands: /file <path>, /files, /clear, /save <path>, /import <path>, /mcp")
	fmt.Fprintln(w)

	tp, isToolProvider := p.(provider.ToolProvider)
	tools := mgr.Tools()

	var pendingAttachments []provider.Attachment

	for {
		input, err := rl.Readline()
		if err != nil { // io.EOF or readline.ErrInterrupt
			fmt.Fprintln(w, "\nBye!")
			return nil
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		// Expand paste tags: [#1 first few chars... N lines] → full pasted text
		input = expandPasteTags(input, pf)

		// Handle commands
		if strings.HasPrefix(input, "/file ") {
			path := strings.TrimSpace(input[6:])
			att, err := ReadAttachment(path)
			if err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", err)
			} else {
				pendingAttachments = append(pendingAttachments, att)
				DimStyle.Fprintf(w, "Attached: %s (%s, %d bytes)\n", att.Filename, att.MimeType, len(att.Data))
			}
			continue
		}
		if input == "/files" {
			fmt.Fprint(w, FormatAttachmentList(pendingAttachments))
			continue
		}
		if input == "/clear" {
			pendingAttachments = nil
			DimStyle.Fprintln(w, "Attachments cleared.")
			continue
		}
		if input == "/save" || strings.HasPrefix(input, "/save ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/save"))
			if path == "" {
				path = "history.md"
			}
			if err := SaveHistory(history, path); err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", err)
			} else {
				DimStyle.Fprintf(w, "Conversation saved to %s\n", path)
			}
			continue
		}
		if input == "/import" || strings.HasPrefix(input, "/import ") {
			path := strings.TrimSpace(strings.TrimPrefix(input, "/import"))
			if path == "" {
				path = "history.md"
			}
			imported, err := ImportHistory(path)
			if err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n", err)
			} else {
				history = imported
				pendingAttachments = nil
				DimStyle.Fprintf(w, "Imported %d messages from %s\n", len(imported), path)
			}
			continue
		}
		if input == "/mcp" || strings.HasPrefix(input, "/mcp ") {
			printMCPStatus(mgr, w)
			continue
		}

		msg := provider.Message{Role: "user", Content: input, Attachments: pendingAttachments}
		pendingAttachments = nil
		history = append(history, msg)

		// Use tool-call loop if provider supports tools and MCP tools are available
		if isToolProvider && len(tools) > 0 {
			reply, thinking, err := executeWithTools(ctx, tp, mgr, &history, tools, w)
			if err != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n\n", err)
				history = history[:len(history)-1]
				continue
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w)
			history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
			continue
		}

		// Standard streaming path (no tools)
		reply, thinking, streamErr := streamResponse(ctx, p, history, w)
		if streamErr != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n\n", streamErr)
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w)
		history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
	}
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

	withSpinner("Thinking...", func() {
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
// final text response.
func executeWithTools(ctx context.Context, tp provider.ToolProvider, mgr *mcpmgr.Manager, history *[]provider.Message, tools []provider.ToolDef, w io.Writer) (string, string, error) {
	// Persistent spinner across all tool-call rounds
	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Writer = os.Stderr
	spinnerRunning := false
	totalCalls := 0
	var toolErrors []string
	var allToolNames []string

	startSpinner := func(suffix string) {
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
			if totalCalls > 0 {
				printToolSummary(w, allToolNames, toolErrors)
			}
			return "", "", readErr
		}

		stopSpinner()

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
				if len(toolCalls) > 0 {
					goto handleToolCalls
				}
				// Reasoning-only response
				if totalCalls > 0 {
					printToolSummary(w, allToolNames, toolErrors)
				}
				return reasoning, reasoning, nil
			}
		}

		// Stream content to display
		if firstN > 0 {
			AssistantStyle.Fprint(w, "Assistant> ")
			mdw := newMarkdownWriter(os.Stdout)
			mdw.Write(firstChunk[:firstN])
			io.Copy(mdw, contentPr)
			mdw.Flush()
		} else {
			io.Copy(io.Discard, contentPr)
		}
		<-done

		if streamErr != nil {
			if totalCalls > 0 {
				printToolSummary(w, allToolNames, toolErrors)
			}
			return "", "", streamErr
		}

		if len(toolCalls) == 0 {
			if totalCalls > 0 {
				fmt.Fprintln(w)
				printToolSummary(w, allToolNames, toolErrors)
			}
			return content, reasoning, nil
		}

	handleToolCalls:
		if content != "" {
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

		// Execute each tool call via MCP with spinner status
		for idx, tc := range toolCalls {
			totalCalls++
			allToolNames = append(allToolNames, tc.Name)

			summary := toolCallSummary(tc, 60)
			if len(toolCalls) > 1 {
				startSpinner(fmt.Sprintf("[%d/%d] %s", idx+1, len(toolCalls), summary))
			} else {
				startSpinner(summary)
			}

			resultText, isError, callErr := mgr.CallTool(ctx, tc.Name, tc.Arguments)
			if callErr != nil {
				resultText = fmt.Sprintf("Error calling tool: %v", callErr)
				isError = true
			}

			if isError {
				toolErrors = append(toolErrors, fmt.Sprintf("%s: %s", tc.Name, truncate(resultText, 100)))
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
		// Keep spinner running into next iteration (Thinking...)
	}

}

func printToolSummary(w io.Writer, names []string, errors []string) {
	// Deduplicate and count tool names
	counts := make(map[string]int)
	var order []string
	for _, name := range names {
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	var parts []string
	for _, name := range order {
		if counts[name] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
		} else {
			parts = append(parts, name)
		}
	}

	if len(errors) > 0 {
		for _, e := range errors {
			ErrorStyle.Fprintf(w, "[tool error: %s]\n", e)
		}
	}
	DimStyle.Fprintf(w, "[%d tool calls: %s]\n", len(names), strings.Join(parts, ", "))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// toolCallSummary returns a compact one-line summary like "tool_name(key1=val1, key2=val2)".
func toolCallSummary(tc provider.ToolCall, maxWidth int) string {
	var params []string
	for k, v := range tc.Arguments {
		s := fmt.Sprintf("%v", v)
		if len(s) > 20 {
			s = s[:20] + "…"
		}
		params = append(params, k+"="+s)
	}
	summary := tc.Name + "(" + strings.Join(params, ", ") + ")"
	if len(summary) > maxWidth {
		summary = summary[:maxWidth] + "…"
	}
	return summary
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
