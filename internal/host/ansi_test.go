package host

import (
	"testing"

	"chatchain/internal/ui"
)

type sinkRec struct {
	progress []ui.ProgressState
	notes    []string
}

func (s *sinkRec) SetProgress(p ui.ProgressState) { s.progress = append(s.progress, p) }
func (s *sinkRec) Notify(text string)             { s.notes = append(s.notes, text) }

// The ANSI host is a pure mapping onto the ui facade: host semantics in,
// renderer-facing progress states and notification text out.
func TestANSIMapping(t *testing.T) {
	rec := &sinkRec{}
	a := NewANSI(rec)

	for _, s := range []State{StateBusy, StateNeedsInput, StateError, StateIdle} {
		a.SetState(s)
	}
	want := []ui.ProgressState{ui.ProgressBusy, ui.ProgressInput, ui.ProgressError, ui.ProgressNone}
	if len(rec.progress) != len(want) {
		t.Fatalf("progress = %v, want %v", rec.progress, want)
	}
	for i := range want {
		if rec.progress[i] != want[i] {
			t.Errorf("state %d → %v, want %v", i, rec.progress[i], want[i])
		}
	}

	a.Notify(Event{Kind: KindDone, Text: "chatchain: The fix"})
	if len(rec.notes) != 1 || rec.notes[0] != "chatchain: The fix" {
		t.Errorf("notes = %v", rec.notes)
	}
}
