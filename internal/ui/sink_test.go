package ui

import (
	"strings"
	"testing"
	"time"
)

// The preview writer METERS the stream instead of forwarding it: the header
// gains a throttled "· N lines" counter and no source line ever enters the
// window — the preview stays exactly one row.
func TestPreviewWriterCounts(t *testing.T) {
	r := &region{emit: func([]string, regionMsg) {}}
	r.openPreview("rendering table…")
	w := &previewWriter{r: r, base: "rendering table…", last: time.Now().Add(-time.Second)}

	w.Write([]byte("| a | b |\n| 1 | 2 |\n| 3 "))
	if len(r.ptail) != 0 {
		t.Fatalf("source lines leaked into the window: %v", r.ptail)
	}
	if !strings.Contains(r.label, "2 lines") {
		t.Fatalf("label = %q, want the 2-line counter", r.label)
	}
	if w.lines != 2 || !w.partial {
		t.Fatalf("count = %d partial = %v", w.lines, w.partial)
	}

	// Within the throttle window the label stays put.
	w.Write([]byte("| 4 |\n"))
	if !strings.Contains(r.label, "2 lines") {
		t.Fatalf("throttle broken: %q", r.label)
	}

	w.Close()
	if r.open {
		t.Fatal("Close must defer-close the preview")
	}
}

// A block that flushes before the first throttle tick never shows a counter
// at all — the short-block case stays visually silent.
func TestPreviewWriterQuietForShortBlocks(t *testing.T) {
	r := &region{emit: func([]string, regionMsg) {}}
	r.openPreview("rendering list…")
	w := &previewWriter{r: r, base: "rendering list…", last: time.Now()}
	w.Write([]byte("- a\n- b\n"))
	w.Close()
	if r.label != "rendering list…" {
		t.Fatalf("short block must keep the bare label, got %q", r.label)
	}
}

func TestCountLines(t *testing.T) {
	if countLines(1) != "1 line" || countLines(5) != "5 lines" {
		t.Fatalf("countLines wording: %q / %q", countLines(1), countLines(5))
	}
}
