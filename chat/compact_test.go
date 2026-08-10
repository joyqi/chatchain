package chat

import (
	"context"
	"strings"
	"testing"

	"chatchain/provider"
)

func TestRetainTailCount(t *testing.T) {
	h := []provider.Message{
		{Role: "system"}, {Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
	}
	if got := retainTailCount(h); got != 2 {
		t.Errorf("retainTailCount = %d, want 2", got)
	}
}

func TestCompactHistory(t *testing.T) {
	p := &stubProvider{model: "m1"}
	h := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
	}
	newHist, summary, retainTail, changed, err := compactHistory(context.Background(), p, h, "")
	if err != nil || !changed {
		t.Fatalf("compactHistory changed=%v err=%v", changed, err)
	}
	if summary != "SUMMARY" || retainTail != 2 {
		t.Fatalf("summary=%q retainTail=%d", summary, retainTail)
	}
	// system + (u2 with summary prepended) + a2
	if len(newHist) != 3 {
		t.Fatalf("newHist len = %d, want 3", len(newHist))
	}
	if newHist[0].Role != "system" {
		t.Errorf("newHist[0] role = %q", newHist[0].Role)
	}
	if !strings.Contains(newHist[1].Content, "SUMMARY") || !strings.Contains(newHist[1].Content, "u2") {
		t.Errorf("newHist[1] content missing summary/u2: %q", newHist[1].Content)
	}
	if newHist[2].Content != "a2" {
		t.Errorf("newHist[2] = %q, want a2", newHist[2].Content)
	}
	// original history must be untouched (first retained is a copy)
	if h[3].Content != "u2" {
		t.Errorf("original history mutated: %q", h[3].Content)
	}
}

// A compaction marker on disk must drive the reconstructed view: system +
// summary-prepended-first-retained + tail, with full log preserved.
func TestCompactionMarkerReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}
	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()
	sw.AppendMessages([]provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
	})
	// Supersede u1,a1 (retain last 2 conv messages: u2,a2).
	if err := sw.AppendCompaction("THE-SUMMARY", 2, nil); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	sw.Close()

	sess, err := LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	v := sess.Messages
	if len(v) != 3 {
		t.Fatalf("view len = %d, want 3 (system + summary-on-u2 + a2): %+v", len(v), v)
	}
	if v[0].Role != "system" || v[0].Content != "sys" {
		t.Errorf("view[0] = %+v", v[0])
	}
	if v[1].Role != "user" || !strings.Contains(v[1].Content, "THE-SUMMARY") || !strings.Contains(v[1].Content, "u2") {
		t.Errorf("view[1] should be u2 with summary prepended: %q", v[1].Content)
	}
	if v[2].Content != "a2" {
		t.Errorf("view[2] = %q, want a2", v[2].Content)
	}

	// Resume must seed convCount from the full log (4 conv msgs), so a further
	// compaction indexes correctly.
	w, _, err := ResumeSession(id, p)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if w.convCount != 4 {
		t.Errorf("resumed convCount = %d, want 4", w.convCount)
	}
	w.Close()
}
