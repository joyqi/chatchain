package chat

import (
	"io"
	"reflect"
	"testing"
)

// recordingSink captures CommitLines batches and preview lifecycle events.
type recordingSink struct {
	batches [][]string
	events  []string // "open:<label>", "close"
}

func (r *recordingSink) CommitLines(lines ...string) {
	batch := append([]string{}, lines...)
	r.batches = append(r.batches, batch)
	r.events = append(r.events, "commit")
}

func (r *recordingSink) BlockPreview(label string) io.WriteCloser {
	r.events = append(r.events, "open:"+label)
	return &recClose{r}
}
func (r *recordingSink) Done() {}

type recClose struct{ r *recordingSink }

func (c *recClose) Write(p []byte) (int, error) { return len(p), nil }
func (c *recClose) Close() error                { c.r.events = append(c.r.events, "close"); return nil }

// TestUIMDSinkBatchesLines pins the anti-crawl contract: all complete lines in
// one Write land as ONE CommitLines batch (a single multi-line insert), so a
// flushed block (e.g. a rendered table after its preview's frame shrink)
// pushes the frame back in one hop instead of row-by-row.
func TestUIMDSinkBatchesLines(t *testing.T) {
	rec := &recordingSink{}
	s := newUIMDSink(rec, func() int { return 80 })

	// A rendered 4-line block arriving in one Write (flushTable's Fprintln).
	if _, err := s.Write([]byte("r1\nr2\nr3\nr4\n")); err != nil {
		t.Fatal(err)
	}
	if len(rec.batches) != 1 || !reflect.DeepEqual(rec.batches[0], []string{"r1", "r2", "r3", "r4"}) {
		t.Fatalf("batches = %v, want one batch of 4 lines", rec.batches)
	}

	// Streaming: a partial line buffers until its newline arrives.
	rec.batches = nil
	s.Write([]byte("hel"))
	if len(rec.batches) != 0 {
		t.Fatalf("partial line committed early: %v", rec.batches)
	}
	s.Write([]byte("lo\nwor"))
	if len(rec.batches) != 1 || rec.batches[0][0] != "hello" {
		t.Fatalf("line not assembled across writes: %v", rec.batches)
	}
	s.flush()
	if len(rec.batches) != 2 || rec.batches[1][0] != "wor" {
		t.Fatalf("flush did not commit the tail: %v", rec.batches)
	}
}

// TestUIMDSinkDefersPreviewClose pins the anti-bounce contract: markdown
// closes a block preview BEFORE rendering the block, but the ui must not see
// the close until the rendered block arrives — shrink and push have to be
// adjacent messages, not separated by the render gap.
func TestUIMDSinkDefersPreviewClose(t *testing.T) {
	rec := &recordingSink{}
	s := newUIMDSink(rec, func() int { return 80 })

	pw := s.BlockPreview("rendering table…")
	pw.Write([]byte("| a | b |\n"))
	pw.Close() // markdown closes, then spends time rendering…
	if len(rec.events) == 0 || rec.events[len(rec.events)-1] == "close" {
		t.Fatalf("close reached the ui before the block: %v", rec.events)
	}
	s.Write([]byte("┌───┐\n│ t │\n└───┘\n")) // …then the rendered block lands
	want := []string{"open:rendering table…", "close", "commit"}
	if len(rec.events) != 3 || rec.events[0] != want[0] || rec.events[1] != want[1] || rec.events[2] != want[2] {
		t.Fatalf("events = %v, want %v (close adjacent to commit)", rec.events, want)
	}

	// Back-to-back blocks: opening the next preview settles the previous close.
	rec.events = nil
	pw = s.BlockPreview("rendering list…")
	pw.Close()
	s.BlockPreview("rendering quote…")
	if len(rec.events) != 3 || rec.events[1] != "close" {
		t.Fatalf("next preview did not settle the pending close: %v", rec.events)
	}
}
