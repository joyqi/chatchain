package chat

import (
	"context"
	"fmt"

	"chatchain/internal/host"
	"chatchain/internal/ui"
)

// approvalGate is the one place a state-changing call is put to the user.
//
// Both the conversation's own tool loop and a delegated child arrive here. A
// child has no terminal of its own, and giving it a second gate would mean
// two prompts with two memories in front of one person — so its questions
// travel up to this one instead, labelled with the agent that asked.
//
// The "allow for this session" memory is shared for the same reason: the
// grant a user gives is "this session may edit files", and a child running
// inside the session is part of it. What the prompt shows is symmetric too —
// the tool and the file, never the diff, which settles only after the call —
// so a parent's request and a child's are answered on the same evidence.
type approvalGate struct {
	u        *ui.UI
	tr       *transcript
	pres     *host.Presenter
	approved map[string]bool // "allow for this session", keyed by tool name
}

// ask resolves one gated call. subject names where the request came from when
// it was not this conversation ("search" for a delegation); empty for the
// conversation's own calls, whose origin needs no saying.
//
// The error is reserved for the prompt itself failing. A refusal is (false,
// nil): the caller turns it into a result the model can read, and a turn that
// continues after a denial is the point of asking.
func (g *approvalGate) ask(ctx context.Context, name, subject string) (bool, error) {
	if g.approved[name] {
		return true, nil
	}
	label := displayToolName(name)
	if subject != "" {
		label = subject + " › " + label
	}
	// The turn is now blocked on the user: needs-input state on every host,
	// and a ping if they wandered off.
	g.pres.SetState(host.StateNeedsInput)
	g.pres.Notify(host.Event{Kind: host.KindNeedsInput,
		Text: fmt.Sprintf("%s wants to modify files", label)})
	g.tr.pauseForInput("waiting for approval")
	choice, err := g.u.Select(ctx, ui.SelectSpec{
		Title: fmt.Sprintf("%s wants to modify files — allow?", label),
		Items: []string{"Allow once", "Allow for this session", "Deny"},
	})
	g.tr.resumeFromInput()
	g.pres.SetState(host.StateBusy) // resolved either way; end states override
	if err != nil {
		return false, err
	}
	if choice.Cancelled || choice.Index == 2 {
		return false, nil
	}
	if choice.Index == 1 {
		g.approved[name] = true
	}
	return true, nil
}
