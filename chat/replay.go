package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"chatchain/internal/imgterm"
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
// not replayed verbatim — each run of tool-result messages collapses into the
// live summary's timeless form, a dim "◇ ran N tool(s)" line — except
// interactive tools (interactive, nil = none), whose results replay as their
// "?" record blocks: those are the user's own answers. Reasoning is never
// replayed. imgDir is the session's images directory ("" = none): a replayed
// image whose file still exists there gets the live turn's clickable-path
// caption.
func echoRounds(w io.Writer, msgs []provider.Message, imgDir string, interactive func(name string) bool) {
	tw := 100
	if width, _, werr := term.GetSize(int(os.Stdout.Fd())); werr == nil && width > 0 {
		tw = width
	}
	// Assistant attachment names in this window: a user attachment with the
	// same name is the /edit canvas copy of an image RENDERED elsewhere in
	// the echo — repeating it as a marker is pure noise. When the canvas's
	// source round fell outside the window the name won't match and the
	// marker rightly shows (then it IS the only trace of the reference).
	rendered := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, a := range m.Attachments {
			if a.Filename != "" {
				rendered[a.Filename] = true
			}
		}
	}
	toolResults := 0
	// flushTools prints the aggregated tool-activity line for the current round
	// once something else is about to print (or the replay ends).
	flushTools := func() {
		if toolResults == 0 {
			return
		}
		DimStyle.Fprintf(w, "%s ran %d %s\n", reasoningSymbol, toolResults, pluralTools(toolResults))
		fmt.Fprintln(w)
		toolResults = 0
	}
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			flushTools()
			printUserBlock(w, msg.Content)
			// Named per-file lines instead of a bare count; canvas copies
			// (see `rendered`) are suppressed. Files still present in the
			// session's images dir get the clickable path.
			for _, att := range msg.Attachments {
				if rendered[att.Filename] {
					continue
				}
				label := att.Filename
				if label == "" {
					label = att.MimeType
				}
				if imgDir != "" && att.Filename != "" {
					if p := filepath.Join(imgDir, att.Filename); fileExists(p) {
						label = markdown.Hyperlink("file://"+p, p)
					}
				}
				DimStyle.Fprintf(w, "📎 %s\n", label)
			}
			fmt.Fprintln(w)
		case "assistant":
			if msg.Content == "" && len(msg.Attachments) == 0 {
				continue // tool-call-only step; its results are counted below
			}
			flushTools()
			// One-shot markdown render: Write consumes complete lines and Flush
			// may leave the trailing line unterminated — close it only when
			// needed, so the round separator is exactly one blank line (the
			// live loop's spacing).
			if msg.Content != "" {
				lw := &lastByteWriter{w: w}
				mdw := markdown.NewWriterTo(lw, tw)
				mdw.Write([]byte(strings.TrimRight(msg.Content, "\n")))
				mdw.Flush()
				if lw.last != '\n' {
					fmt.Fprintln(w)
				}
			}
			// Generated images re-render as half-blocks, mirroring the live
			// turn (same size caps and indent); a decode failure or non-image
			// attachment falls back to the name-only line.
			for i, att := range msg.Attachments {
				if msg.Content != "" || i > 0 {
					fmt.Fprintln(w)
				}
				echoImage(w, att, tw, imgDir)
			}
			if msg.Interrupted {
				DimStyle.Fprintln(w, "(interrupted)")
			}
			fmt.Fprintln(w)
		case "tool":
			if interactive != nil && interactive(msg.ToolCallName) {
				// The user's own answers replay as the "?" record block.
				flushTools()
				for i, ln := range strings.Split(strings.TrimRight(msg.Content, "\n"), "\n") {
					prefix := "  "
					if i == 0 {
						prefix = "? "
					}
					DimStyle.Fprintf(w, "%s%s\n", prefix, ln)
				}
				fmt.Fprintln(w)
				continue
			}
			toolResults++
		}
	}
	flushTools()
}

// echoImage renders one replayed attachment: half-block rows plus a dim
// caption, with the live path's indent and size caps. When the image file
// still exists under imgDir the caption is the live turn's clickable path
// (OSC 8); otherwise it falls back to the bare filename.
func echoImage(w io.Writer, att provider.Attachment, termWidth int, imgDir string) {
	indent := strings.Repeat(" ", imageIndentCols)
	caption := att.Filename
	if imgDir != "" && att.Filename != "" {
		if p := filepath.Join(imgDir, att.Filename); fileExists(p) {
			caption = markdown.Hyperlink("file://"+p, p)
		}
	}
	if !strings.HasPrefix(att.MimeType, "image/") || len(att.Data) == 0 {
		DimStyle.Fprintf(w, "%s🖼 %s\n", indent, caption)
		return
	}
	maxCols := imageMaxCols
	if termWidth-2-imageIndentCols < maxCols {
		maxCols = termWidth - 2 - imageIndentCols
	}
	rows, err := imgterm.Render(att.Data, maxCols, imageMaxRows)
	if err != nil {
		DimStyle.Fprintf(w, "%s🖼 %s\n", indent, caption)
		return
	}
	for _, row := range rows {
		fmt.Fprintln(w, indent+row)
	}
	DimStyle.Fprintf(w, "%s🖼 %s\n", indent, caption)
}

// fileExists reports whether path names an existing regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// lastByteWriter tracks the final byte written, so the replay can tell an
// already-terminated markdown render from a dangling partial line.
type lastByteWriter struct {
	w    io.Writer
	last byte
}

func (l *lastByteWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		l.last = p[len(p)-1]
	}
	return l.w.Write(p)
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
// Embedded newlines START A ROW, they are not runes to measure: folding them
// into the width count let a multi-line message render as one "row" carrying
// its own line breaks — the terminal then broke it at column 0 (no gutter)
// and the padding was computed against the whole blob, so the block's
// background stopped at the text. The ui-side twin has always split first.
func wrapByWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		var b strings.Builder
		cur := 0
		for _, r := range line {
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
	}
	return rows
}
