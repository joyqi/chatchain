package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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

func Once(ctx context.Context, p provider.Provider, message string, systemPrompt string, w io.Writer) error {
	var messages []provider.Message
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, provider.Message{Role: "user", Content: message})
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
		commands := []string{"/file ", "/files ", "/clear ", "/save ", "/import "}
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

func Run(p provider.Provider, systemPrompt string, importedHistory []provider.Message, w io.Writer) error {
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
	DimStyle.Fprintln(w, "Commands: /file <path>, /files, /clear, /save <path>, /import <path>")
	fmt.Fprintln(w)

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

		msg := provider.Message{Role: "user", Content: input, Attachments: pendingAttachments}
		pendingAttachments = nil
		history = append(history, msg)

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

		// Spinner blocks until first byte from reasoning or content
		withSpinner("Thinking...", func() {
			firstN, readErr = reasonPr.Read(firstChunk)
			if readErr != nil {
				// No reasoning (EOF), try content
				readErr = nil
				firstN, readErr = contentPr.Read(firstChunk)
			} else {
				hasReasoning = true
			}
		})

		if readErr != nil {
			<-done
			if streamErr != nil {
				ErrorStyle.Fprintf(w, "Error: %v\n\n", streamErr)
			} else {
				ErrorStyle.Fprintf(w, "Error: %v\n\n", readErr)
			}
			history = history[:len(history)-1]
			continue
		}

		if hasReasoning {
			// Display reasoning in dim style
			DimStyle.Fprint(w, "Reasoning> ")
			os.Stdout.WriteString("\033[2m")
			os.Stdout.Write(firstChunk[:firstN])
			io.Copy(os.Stdout, reasonPr)
			os.Stdout.WriteString("\033[0m")
			fmt.Fprintln(w)

			// Now read first content chunk
			firstN, readErr = contentPr.Read(firstChunk)
			if readErr != nil {
				// Reasoning-only response (no text content) — treat as valid
				<-done
				if streamErr != nil {
					ErrorStyle.Fprintf(w, "Error: %v\n\n", streamErr)
					history = history[:len(history)-1]
					continue
				}
				fmt.Fprintln(w)
				history = append(history, provider.Message{Role: "assistant", Content: thinking, Reasoning: thinking})
				continue
			}
		}

		AssistantStyle.Fprint(w, "Assistant> ")
		os.Stdout.Write(firstChunk[:firstN])
		io.Copy(os.Stdout, contentPr)
		<-done

		if streamErr != nil {
			ErrorStyle.Fprintf(w, "\nError: %v\n\n", streamErr)
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w)
		history = append(history, provider.Message{Role: "assistant", Content: reply, Reasoning: thinking})
	}
}
