package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"chatchain/provider"

	"github.com/briandowns/spinner"
	"github.com/manifoldco/promptui"
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

func Once(ctx context.Context, p provider.Provider, message string, w io.Writer) error {
	messages := []provider.Message{{Role: "user", Content: message}}
	reply, err := p.Chat(ctx, messages)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, reply)
	return nil
}

func Run(p provider.Provider, w io.Writer) error {
	var history []provider.Message
	ctx := context.Background()

	fmt.Fprintln(w, "Chat started. Press Ctrl+C to exit.")
	fmt.Fprintln(w)

	scanner := bufio.NewScanner(os.Stdin)

	for {
		UserStyle.Fprint(w, "You> ")

		if !scanner.Scan() {
			fmt.Fprintln(w, "\nBye!")
			return nil
		}

		input := strings.TrimSpace(scanner.Text())
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

		withSpinner("Thinking...", func() {
			firstN, readErr = pr.Read(firstChunk)
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

		AssistantStyle.Fprint(w, "Assistant> ")
		os.Stdout.Write(firstChunk[:firstN])
		io.Copy(os.Stdout, pr)
		<-done

		if streamErr != nil {
			ErrorStyle.Fprintf(w, "\nError: %v\n\n", streamErr)
			history = history[:len(history)-1]
			continue
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w)
		history = append(history, provider.Message{Role: "assistant", Content: reply})
	}
}
