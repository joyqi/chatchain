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

	statusMsg StatusData
	titleMsg  string

	busyOnMsg  struct{ label string }
	busyOffMsg struct{}

	scopePushMsg struct{ cancel context.CancelFunc } // a turn/tool cancel scope
	scopePopMsg  struct{}

	selectOpenMsg struct {
		spec  SelectSpec
		reply chan SelectResult
	}
	viewOpenMsg struct {
		spec  ViewSpec
		reply chan struct{}
	}
	surfaceCancelMsg struct{} // caller ctx died: close whatever surface is open

	spinTickMsg struct{}
)
