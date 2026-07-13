package ui

import (
	"io"
	"strings"
)

// streamSink is the StreamSink implementation: fire-and-forget messages into
// the Program; ordering rides the Program mailbox (FIFO), so committed lines,
// preview updates, and the closing Done arrive in call order.
type streamSink struct {
	u *UI
}

func (s *streamSink) CommitLines(lines ...string) {
	if len(lines) == 0 {
		return
	}
	s.u.insert(strings.Join(lines, "\n"))
}

func (s *streamSink) BlockPreview(label string) io.WriteCloser {
	s.u.p.Send(previewOpenMsg{label: label})
	return &previewWriter{u: s.u}
}

func (s *streamSink) Done() {
	s.u.p.Send(previewCloseMsg{}) // belt and braces: a leaked preview dies with the turn
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
		w.u.p.Send(previewLineMsg{line: string(w.buf[:i])})
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *previewWriter) Close() error {
	if len(w.buf) > 0 {
		w.u.p.Send(previewLineMsg{line: string(w.buf)})
		w.buf = nil
	}
	w.u.p.Send(previewCloseMsg{})
	return nil
}
