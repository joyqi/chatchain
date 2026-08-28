package chat

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/joyqi/iota/internal/ui"
)

// uiMDSink adapts the ui to markdown.Sink for the interactive loop: the
// renderer's committed output is line-buffered and lands in scrollback via
// commit (the transcript's content block, which owns the spacing); block
// previews pass straight through to the sink — after materializing any
// latched separator via preOpen, so the preview is spaced exactly like the
// block it morphs into; the layout width is the ui's live width. flush
// commits any final partial line (call it after markdown.Writer.Flush).
type uiMDSink struct {
	sink    ui.StreamSink
	commit  func(lines ...string)
	width   func() int
	preOpen func() // nil-safe: transcript.flushPending
	buf     []byte
}

func newUIMDSink(sink ui.StreamSink, commit func(lines ...string), width func() int, preOpen func()) *uiMDSink {
	return &uiMDSink{sink: sink, commit: commit, width: width, preOpen: preOpen}
}

// Write commits every complete line in the chunk as ONE CommitLines batch —
// a single multi-line insert. Per-line commits would make a flushed block
// (a rendered table following its preview's frame shrink) crawl back row by
// row: the shrink pops the composer up and nine separate inserts walk it back
// down. One batched insert makes shrink+push land back to back, within a
// render frame.
func (s *uiMDSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	var lines []string
	for {
		i := -1
		for j, b := range s.buf {
			if b == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		lines = append(lines, string(s.buf[:i]))
		s.buf = s.buf[i+1:]
	}
	if len(lines) > 0 {
		s.commit(lines...)
	}
	return len(p), nil
}

// flush commits a trailing partial line left in the buffer.
func (s *uiMDSink) flush() {
	if len(s.buf) > 0 {
		s.commit(string(s.buf))
		s.buf = nil
	}
}

func (s *uiMDSink) Width() int { return s.width() }

func (s *uiMDSink) BlockPreview(label string) io.WriteCloser {
	// The block's separator may sit in the transcript's blank latch (interior
	// blanks defer until more content follows) — the preview about to occupy
	// the next row IS that content, so materialize it first: the preview must
	// be spaced exactly like the block it morphs into.
	if s.preOpen != nil {
		s.preOpen()
	}
	// No close deferral needed here: the ui's staging window (region.go)
	// keeps a closed preview on screen until the rendered block replaces it in
	// place — the close is visually free by construction. The ui renders
	// labels as given, so markdown's plain "rendering…" labels stay dim.
	return s.sink.BlockPreview(dim(label))
}

// lineCommitter is an io.Writer that buffers formatted output and commits it
// as lines through commit (a transcript verb: toolLines, noticeLines) — the
// v2 shape for helpers that print multi-line output into an io.Writer.
type lineCommitter struct {
	commit func(lines ...string)
	buf    strings.Builder
}

func (l *lineCommitter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	return len(p), nil
}

// flush commits everything buffered (split on newlines, trailing newline
// dropped) as one batch. fatih/color's Fprintf wraps the SGR reset AROUND a
// trailing newline in the format string ("\x1b[2m…\n\x1b[0m"), leaving
// reset-only "lines" that would render as spurious blank rows — any
// escape-only line is glued back onto its predecessor so every committed line
// stays self-contained.
func (l *lineCommitter) flush() {
	s := strings.TrimSuffix(l.buf.String(), "\n")
	l.buf.Reset()
	if s == "" {
		return
	}
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if len(out) > 0 && ln != "" && ansi.Strip(ln) == "" {
			out[len(out)-1] += ln
			continue
		}
		out = append(out, ln)
	}
	l.commit(out...)
}
