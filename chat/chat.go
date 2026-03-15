package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"chatchain/provider"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
)

func SelectModel(models []string) (string, error) {
	options := make([]huh.Option[string], len(models))
	for i, m := range models {
		options[i] = huh.NewOption(m, m)
	}

	var selected string
	err := huh.NewSelect[string]().
		Title("Select a model").
		Options(options...).
		Value(&selected).
		Run()
	if err != nil {
		return "", err
	}
	return selected, nil
}

func Run(p provider.Provider, w io.Writer) error {
	var history []provider.Message
	ctx := context.Background()

	fmt.Fprintln(w, "Chat started. Press Ctrl+C or Esc to exit.")
	fmt.Fprintln(w)

	for {
		var input string
		err := huh.NewInput().
			Title("You>").
			Value(&input).
			Run()
		if err != nil {
			fmt.Fprintln(w, "\nBye!")
			return nil
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		history = append(history, provider.Message{Role: "user", Content: input})

		pr, pw := io.Pipe()
		var reply string
		var streamErr error
		done := make(chan struct{})

		go func() {
			defer close(done)
			defer pw.Close()
			reply, streamErr = p.StreamChat(ctx, history, pw)
		}()

		firstChunk := make([]byte, 4096)
		var firstN int
		var readErr error

		_ = spinner.New().
			Title("Thinking...").
			Action(func() {
				firstN, readErr = pr.Read(firstChunk)
			}).
			Run()

		if readErr != nil {
			<-done
			if streamErr != nil {
				fmt.Fprintf(w, "%s\n\n", ErrorStyle.Render(fmt.Sprintf("Error: %v", streamErr)))
			} else {
				fmt.Fprintf(w, "%s\n\n", ErrorStyle.Render(fmt.Sprintf("Error: %v", readErr)))
			}
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprint(w, AssistantStyle.Render("Assistant>")+" ")
		os.Stdout.Write(firstChunk[:firstN])
		io.Copy(os.Stdout, pr)
		<-done

		if streamErr != nil {
			fmt.Fprintf(w, "\n%s\n\n", ErrorStyle.Render(fmt.Sprintf("Error: %v", streamErr)))
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w)
		history = append(history, provider.Message{Role: "assistant", Content: reply})
	}
}
