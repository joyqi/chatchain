package chat

import (
	"io"
	"os"

	"chatchain/internal/markdown"
	"chatchain/internal/promptui"

	"golang.org/x/term"
)

// mdSink is the readline-path implementation of markdown.Sink: committed
// rendered lines go to w, the layout width is the live terminal width, and
// block previews are promptui.StreamView rolling windows — exactly the
// pre-extraction behavior (previews only when w is a terminal *os.File).
type mdSink struct {
	w       io.Writer
	preview bool
}

func newMDSink(w io.Writer) mdSink {
	f, ok := w.(*os.File)
	return mdSink{w: w, preview: ok && term.IsTerminal(int(f.Fd()))}
}

func (s mdSink) Write(p []byte) (int, error) { return s.w.Write(p) }

// Width returns the live terminal width (0 lets the renderer fall back to 80)
// — queried per block flush, so a mid-stream resize shapes the next block.
func (s mdSink) Width() int {
	tw, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || tw <= 0 {
		return 0
	}
	return tw
}

// BlockPreview opens a StreamView rolling window (spinner header + last 3 raw
// source lines, dim) for a buffering block; nil on non-terminal output so
// piped output stays clean.
func (s mdSink) BlockPreview(label string) io.WriteCloser {
	if !s.preview {
		return nil
	}
	sv := &promptui.StreamView{
		Spinner:     spinnerFrames,
		Label:       label,
		HeaderStyle: dim,
		Window:      3,
		Indent:      "  ",
		RuneWidth:   runeWidth,
		Style:       dim,
		Stdout:      s.w,
	}
	return svPreview{sv}
}

// svPreview adapts StreamView to the io.WriteCloser shape markdown.Sink wants:
// Close finalizes and clears the rolling preview (StreamView.Done).
type svPreview struct{ sv *promptui.StreamView }

func (p svPreview) Write(b []byte) (int, error) { return p.sv.Write(b) }
func (p svPreview) Close() error                { p.sv.Done(""); return nil }

// newMarkdownWriter keeps the historical constructor shape for the chat
// package: a markdown Writer rendering through the readline-path sink.
func newMarkdownWriter(w io.Writer) *markdown.Writer {
	return markdown.NewWriter(newMDSink(w))
}
