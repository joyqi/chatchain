package chat

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"chatchain/internal/markdown"
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
			tw := 100
			if width, _, werr := term.GetSize(int(os.Stdout.Fd())); werr == nil && width > 0 {
				tw = width
			}
			mdw := markdown.NewWriterTo(w, tw)
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

// printUserBlock renders a user message as a stack of full-width reversed
// blocks. A two-column gutter keeps "❯ " on the first row (so the block still
// reads as a prompt) and aligns wrapped rows under it; padding fills the row to
// the full width. Unlike rewriteUserMessage it does no cursor movement, so it
// also serves replayed history on session resume.
func printUserBlock(w io.Writer, display string) {
	tw, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || tw <= 0 {
		tw = 80
	}
	gutterWidth := displayWidth(userPrompt)
	lines := wrapByWidth(display, tw-gutterWidth)
	for i, line := range lines {
		gutter := strings.Repeat(" ", gutterWidth)
		if i == 0 {
			gutter = userPrompt
		}
		pad := tw - displayWidth(line) - gutterWidth
		if pad < 0 {
			pad = 0
		}
		fmt.Fprint(w, UserBlockStyle.Sprintf("%s%s%s", gutter, line, strings.Repeat(" ", pad)))
		fmt.Fprint(w, "\n")
	}
}

// wrapByWidth hard-wraps plain text into rows whose display width is at most
// width, breaking on rune boundaries (mirroring how a terminal wraps). CJK runes
// count as width 2, so a wide rune is never split across rows.
func wrapByWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var rows []string
	var b strings.Builder
	cur := 0
	for _, r := range s {
		rw := runeWidth(r)
		if cur+rw > width {
			rows = append(rows, b.String())
			b.Reset()
			cur = 0
		}
		b.WriteRune(r)
		cur += rw
	}
	rows = append(rows, b.String())
	return rows
}
