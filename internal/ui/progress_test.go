package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The facade's progress states map onto the OSC 9;4 wire states through the
// View, so the renderer owns emission, diffing, and exit cleanup — the model
// never touches raw escapes.
func TestProgressStates(t *testing.T) {
	m := newTestModel(t)
	if v := m.View(); v.ProgressBar != nil {
		t.Fatal("idle model advertises a progress bar")
	}
	if v := m.View(); !v.ReportFocus {
		t.Fatal("focus reporting is off — Notify gating would never see blur")
	}

	for state, want := range map[ProgressState]tea.ProgressBarState{
		ProgressBusy:  tea.ProgressBarIndeterminate,
		ProgressInput: tea.ProgressBarWarning,
		ProgressError: tea.ProgressBarError,
	} {
		m = step(t, m, progressMsg(state))
		pb := m.View().ProgressBar
		if pb == nil || pb.State != want {
			t.Errorf("state %v: ProgressBar = %+v, want %v", state, pb, want)
		}
	}

	m = step(t, m, progressMsg(ProgressNone))
	if v := m.View(); v.ProgressBar != nil {
		t.Error("ProgressNone must clear the bar")
	}
}

// Notify is focus-gated: silent while the terminal is focused (whoever is
// watching needs no bell), both standard channels in one write while blurred
// — the OSC 9 desktop notification then a rung BEL. The global on/off
// switch lives in internal/host.Presenter, not here.
func TestNotifyFocusGating(t *testing.T) {
	m := newTestModel(t)
	var out strings.Builder
	m.notifyOut = &out

	ping := func(text string) {
		t.Helper()
		_, cmd := m.Update(notifyMsg(text))
		if cmd != nil {
			cmd()
		}
	}

	ping("approval needed") // focused (the default): silent
	if out.Len() != 0 {
		t.Fatalf("focused terminal got %q, want silence", out.String())
	}

	m = step(t, m, tea.BlurMsg{})
	ping("approval needed")
	// One write, two signals: the OSC 9 notification (its BEL only
	// terminates the sequence) then the ringing BEL.
	if got := out.String(); !strings.HasPrefix(got, "\x1b]9;approval needed") || !strings.HasSuffix(got, "\x07\a") {
		t.Fatalf("blurred ping = %q, want OSC 9 + BEL", got)
	}

	// A digest opening "4;" would parse as a progress report — the
	// mechanism (not call-site convention) must keep it a notification.
	out.Reset()
	ping("4; things to fix")
	if got := out.String(); !strings.HasPrefix(got, "\x1b]9; 4;") {
		t.Fatalf("collision-prone ping = %q, want a leading space defusing 9;4", got)
	}

	m = step(t, m, tea.FocusMsg{})
	out.Reset()
	ping("done")
	if out.Len() != 0 {
		t.Fatalf("refocused terminal got %q, want silence", out.String())
	}
}
