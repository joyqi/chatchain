package ui

import "context"

// Facade → model messages. Reply channels are ALWAYS buffered(1) and the
// model's sends are non-blocking by construction — a waiter that gave up
// (ctx cancelled) must never deadlock the event loop.

type inputResult struct {
	in  Input
	err error
}

type (
	readReqMsg    struct{ reply chan inputResult }
	readCancelMsg struct{ reply chan inputResult } // revoke a waiter that gave up

	statusMsg      StatusData
	titleMsg       string
	setCommandsMsg []string // slash command table for suggestions/completion

	progressMsg ProgressState
	notifyMsg   string // attention ping: reaches the terminal only while unfocused

	busyOnMsg     struct{ label string }
	busyDetailMsg string
	busyOffMsg    struct{}

	scopePushMsg struct{ cancel context.CancelFunc } // a turn/tool cancel scope
	scopePopMsg  struct{}

	tabbedOpenMsg struct {
		spec  TabbedSpec
		reply chan TabbedResult
	}
	surfaceCancelMsg struct{} // caller ctx died: close whatever surface is open
	surfTickMsg      struct{ gen int }

	spinTickMsg struct{}
)
