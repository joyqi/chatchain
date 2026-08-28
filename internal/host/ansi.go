package host

import "github.com/joyqi/iota/internal/ui"

// UISink is the slice of the ui facade the ANSI host speaks — an interface
// so tests record instead of driving a live Program.
type UISink interface {
	SetProgress(ui.ProgressState)
	Notify(text string)
}

// ANSI is the default host: the plain terminal, spoken to through the ui
// facade — the OSC 9;4 progress indicator rides the bubbletea renderer, and
// the attention ping is the OSC 9 + BEL pair, focus-gated inside the ui
// model where the terminal's focus reporting lives (see the Notifier
// contract in host.go).
type ANSI struct{ u UISink }

// NewANSI wraps the ui facade as the fallback host.
func NewANSI(u UISink) *ANSI { return &ANSI{u: u} }

func (a *ANSI) Name() string { return "terminal" }

func (a *ANSI) SetState(s State) {
	var ps ui.ProgressState
	switch s {
	case StateBusy:
		ps = ui.ProgressBusy
	case StateNeedsInput:
		ps = ui.ProgressInput
	case StateError:
		ps = ui.ProgressError
	default:
		ps = ui.ProgressNone
	}
	a.u.SetProgress(ps)
}

func (a *ANSI) Notify(e Event) { a.u.Notify(e.Text) }
