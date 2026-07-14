package ui

import (
	"io"
)

// streamSink is the StreamSink implementation: fire-and-forget messages into
// the Program; ordering rides the Program mailbox (FIFO), so committed lines,
// preview updates, and the closing Done arrive in call order.
type streamSink struct {
	u *UI
}

func (s *streamSink) CommitLines(lines ...string) {
	s.u.region.commit(lines)
}

func (s *streamSink) BlockPreview(label string) io.WriteCloser {
	s.u.region.openPreview(label)
	return &previewWriter{u: s.u}
}

func (s *streamSink) Done() {
	s.u.region.dropPreview() // a leaked/deferred preview dies with the turn
	s.u.p.Send(scopePopMsg{})
}

// previewWriter feeds raw source lines into the frame's live rolling preview.
// Partial lines are buffered until their newline arrives; Close clears the
// preview (StreamView.Done semantics).
type previewWriter struct {
	u   *UI
	buf []byte
}

func (w *previewWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := -1
		for j, b := range w.buf {
			if b == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		w.u.region.previewLine(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Close marks the preview finished; the window keeps showing it until the
// rendered block flows through and replaces it in place (region.commit).
func (w *previewWriter) Close() error {
	if len(w.buf) > 0 {
		w.u.region.previewLine(string(w.buf))
		w.buf = nil
	}
	w.u.region.closePreview()
	return nil
}
