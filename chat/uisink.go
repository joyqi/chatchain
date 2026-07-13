package chat

import (
	"io"
	"strings"

	"chatchain/internal/ui"
)

// uiMDSink adapts a ui.StreamSink to markdown.Sink for the --ui=v2 path: the
// renderer's committed output is line-buffered and lands in scrollback via
// CommitLines; block previews pass straight through; the layout width is the
// ui's live width. flush commits any final partial line (call it after
// markdown.Writer.Flush).
type uiMDSink struct {
	sink  ui.StreamSink
	width func() int
	buf   []byte
}

func newUIMDSink(sink ui.StreamSink, width func() int) *uiMDSink {
	return &uiMDSink{sink: sink, width: width}
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
		s.sink.CommitLines(lines...)
	}
	return len(p), nil
}

// flush commits a trailing partial line left in the buffer.
func (s *uiMDSink) flush() {
	if len(s.buf) > 0 {
		s.sink.CommitLines(string(s.buf))
		s.buf = nil
	}
}

func (s *uiMDSink) Width() int { return s.width() }

func (s *uiMDSink) BlockPreview(label string) io.WriteCloser {
	return s.sink.BlockPreview(label)
}

// lineCommitter is an io.Writer that buffers formatted output and commits it
// as lines through commit (sink.CommitLines or ui.PrintLines) — the v2 shape
// for helpers that print multi-line output into an io.Writer.
type lineCommitter struct {
	commit func(lines ...string)
	buf    strings.Builder
}

func (l *lineCommitter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	return len(p), nil
}

// flush commits everything buffered (split on newlines, trailing newline
// dropped) as one batch.
func (l *lineCommitter) flush() {
	s := strings.TrimSuffix(l.buf.String(), "\n")
	l.buf.Reset()
	if s == "" {
		return
	}
	l.commit(strings.Split(s, "\n")...)
}
