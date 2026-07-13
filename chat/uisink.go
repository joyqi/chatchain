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
	// pendingClose holds a closed-but-not-yet-cleared block preview. The
	// preview lives ABOVE the composer, so clearing it shrinks the frame and
	// pops the composer up until the block's commit pushes it back; markdown
	// closes the preview BEFORE rendering the block, which would expose that
	// pop for the whole render time. Deferring the clear until the rendered
	// block arrives makes shrink and push adjacent messages — the bounce
	// window collapses to at most one render frame.
	pendingClose io.Closer
}

func newUIMDSink(sink ui.StreamSink, width func() int) *uiMDSink {
	return &uiMDSink{sink: sink, width: width}
}

// settleClose clears a deferred preview right before the next output lands.
func (s *uiMDSink) settleClose() {
	if s.pendingClose != nil {
		s.pendingClose.Close()
		s.pendingClose = nil
	}
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
		s.settleClose() // clear a deferred preview right before its block lands
		s.sink.CommitLines(lines...)
	}
	return len(p), nil
}

// flush commits a trailing partial line left in the buffer.
func (s *uiMDSink) flush() {
	s.settleClose()
	if len(s.buf) > 0 {
		s.sink.CommitLines(string(s.buf))
		s.buf = nil
	}
}

func (s *uiMDSink) Width() int { return s.width() }

func (s *uiMDSink) BlockPreview(label string) io.WriteCloser {
	s.settleClose() // back-to-back blocks: clear the previous preview first
	return &deferredPreview{inner: s.sink.BlockPreview(label), owner: s}
}

// deferredPreview passes writes through to the ui preview but hands its Close
// to the owner as a pendingClose, settled just before the next output.
type deferredPreview struct {
	inner io.WriteCloser
	owner *uiMDSink
}

func (d *deferredPreview) Write(p []byte) (int, error) {
	if d.inner == nil {
		return len(p), nil
	}
	return d.inner.Write(p)
}

func (d *deferredPreview) Close() error {
	if d.inner != nil {
		d.owner.pendingClose = d.inner
		d.inner = nil
	}
	return nil
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
