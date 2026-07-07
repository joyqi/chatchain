package chat

import (
	"fmt"
	"io"
	"strings"

	"chatchain/provider"
)

// resumeEchoRounds is how many trailing conversation rounds a resumed session
// replays to the terminal so the user sees where they left off.
const resumeEchoRounds = 3

// lastRounds returns the tail of history covering at most n rounds; a round
// starts at a user message and runs until the next user message. System
// messages are excluded. When history holds fewer than n rounds the whole
// non-system tail is returned.
func lastRounds(history []provider.Message, n int) []provider.Message {
	if n <= 0 {
		return nil
	}
	start := 0
	rounds := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			rounds++
			if rounds == n {
				start = i
				break
			}
		}
	}
	var out []provider.Message
	for _, m := range history[start:] {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}

// echoRounds replays msgs (as returned by lastRounds) to w, mirroring the live
// chat's layout: user messages as full-width blocks, assistant replies through
// the markdown renderer, and a blank line between messages. Tool activity is
// not replayed verbatim — each round's tool-result messages collapse into one
// dim "⚙ N tool call(s)" line. Reasoning is never replayed.
func echoRounds(w io.Writer, msgs []provider.Message) {
	toolResults := 0
	// flushTools prints the aggregated tool-activity line for the current round
	// once something else is about to print (or the replay ends).
	flushTools := func() {
		if toolResults == 0 {
			return
		}
		DimStyle.Fprintf(w, "⚙ %d tool call(s)\n", toolResults)
		fmt.Fprintln(w)
		toolResults = 0
	}
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			flushTools()
			printUserBlock(w, msg.Content)
			if n := len(msg.Attachments); n > 0 {
				DimStyle.Fprintf(w, "(%d attachment(s))\n", n)
			}
			fmt.Fprintln(w)
		case "assistant":
			if msg.Content == "" {
				continue // tool-call-only step; its results are counted below
			}
			flushTools()
			// One-shot markdown render: Write consumes complete lines and Flush
			// emits the trailing partial line without a newline, so close the
			// last line explicitly (the live loop does the same after a stream).
			mdw := newMarkdownWriter(w)
			mdw.Write([]byte(strings.TrimRight(msg.Content, "\n")))
			mdw.Flush()
			fmt.Fprintln(w)
			if msg.Interrupted {
				DimStyle.Fprintln(w, "(interrupted)")
			}
			fmt.Fprintln(w)
		case "tool":
			toolResults++
		}
	}
	flushTools()
}
