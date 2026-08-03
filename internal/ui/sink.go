package ui

import (
	"fmt"
	"io"
	"time"
)

// streamSink is the StreamSink implementation: fire-and-forget messages into
// the Program; ordering rides the Program mailbox (FIFO), so committed lines,
// preview updates, and the closing Done arrive in call order.
type streamSink struct {
	u *UI
}

func (s *streamSink) BlockPreview(label string) io.WriteCloser {
	s.u.region.openPreview(label)
	// last starts NOW: the first counter update waits a full throttle tick,
	// so a block that flushes quickly never even shows one — zero churn for
	// the short-block case the single-row preview exists to protect.
	return &previewWriter{r: &s.u.region, base: label, last: time.Now()}
}

func (s *streamSink) Done() {
	s.u.region.dropPreview() // a leaked/deferred preview dies with the turn
	s.u.p.Send(scopePopMsg{})
}

// previewCounterEvery throttles the preview header's line counter, mirroring
// the thinking meter's cadence.
const previewCounterEvery = 150 * time.Millisecond

// previewWriter meters the source streaming into a block preview: the
// preview is a SINGLE header row ("rendering table… · 37 lines"), never a
// rolling window of raw source. A one-row preview is always covered by the
// rendered block's morph, so the residue/shrink class — a short list
// collapsing under its own preview at end of turn — cannot occur.
type previewWriter struct {
	r       *region
	base    string
	lines   int
	partial bool
	last    time.Time
}

func (w *previewWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			w.lines++
			w.partial = false
		} else {
			w.partial = true
		}
	}
	if w.lines > 0 && time.Since(w.last) >= previewCounterEvery {
		w.last = time.Now()
		w.r.relabelPreview(w.base + faint + " · " + countLines(w.lines) + sgrReset)
	}
	return len(p), nil
}

func countLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// Close marks the preview finished; the window keeps showing it until the
// rendered block flows through and replaces it in place (region.commit).
func (w *previewWriter) Close() error {
	w.r.closePreview()
	return nil
}
