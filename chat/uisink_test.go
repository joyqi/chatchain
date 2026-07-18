package chat

import (
	"io"
	"reflect"
	"strings"
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
	s := newUIMDSink(rec, rec.CommitLines, func() int { return 80 })

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

// contentTap: every byte reaches the history buffer; bytes after the pipe cut
// spill instead of vanishing; the first byte fires mark exactly once.
func TestContentTap(t *testing.T) {
	pr, pw := io.Pipe()
	var buf strings.Builder
	marks := 0
	tap := &contentTap{pw: pw, buf: &buf, mark: func() { marks++ }}

	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(pr)
		got <- string(b)
	}()

	tap.Write([]byte("before "))
	tap.Write([]byte("cut"))
	pw.Close() // the observer cuts the render pipe at the first tool delta
	tap.Write([]byte(" after"))

	if piped := <-got; piped != "before cut" {
		t.Fatalf("piped = %q, want the pre-cut bytes only", piped)
	}
	if buf.String() != "before cut after" {
		t.Fatalf("history buf = %q, want every byte", buf.String())
	}
	if tap.spill.String() != " after" {
		t.Fatalf("spill = %q, want the post-cut bytes", tap.spill.String())
	}
	if marks != 1 {
		t.Fatalf("mark fired %d times, want 1", marks)
	}
}
