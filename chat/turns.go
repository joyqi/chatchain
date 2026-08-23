package chat

import (
	"context"
	"sync"
)

// The run's turn budget: --max-turns, counted once for everything the run
// does rather than once per agent.
//
// The flag exists for the unattended case — it is -m only — and that is
// exactly where delegation moves the spending somewhere the flag could not
// see. A cap that bounded the parent to five rounds while each of its
// children ran an uncapped loop of its own was not a cap; the number the user
// wrote had no relationship to what the run could cost.
//
// A shared pool is what "limit this run" means, and it is the shape Claude
// Code settled on for the same problem: one budget owned by the run and
// decremented by every agent in it. No number is invented here — a run
// without --max-turns has no budget at all, because a cap nobody chose is
// either too low to be safe or too high to be a cap.

type turnBudget struct {
	mu        sync.Mutex
	remaining int
	total     int
}

// newTurnBudget returns nil for "no cap", so the unlimited case costs nothing
// and needs no branch at the call site (a nil budget always grants).
func newTurnBudget(n int) *turnBudget {
	if n <= 0 {
		return nil
	}
	return &turnBudget{remaining: n, total: n}
}

// take claims one round, reporting false once the run is spent. It is called
// from several goroutines at once: parallel delegations run concurrently, and
// each child's loop draws on this same pool.
func (b *turnBudget) take() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// cap reports the budget the run was given, for the error that announces it
// is gone.
func (b *turnBudget) cap() int {
	if b == nil {
		return 0
	}
	return b.total
}

// turnBudgetKey carries the run's budget to a delegated child. The context is
// the only thing that reaches it: the child is started by a tool, and a tool
// must not know what a turn budget is.
type turnBudgetKey struct{}

func withTurnBudget(ctx context.Context, b *turnBudget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, turnBudgetKey{}, b)
}

// turnBudgetFrom recovers the run's budget. Absent — an interactive run, where
// the brake is the user — it returns nil and every round is granted.
func turnBudgetFrom(ctx context.Context) *turnBudget {
	b, _ := ctx.Value(turnBudgetKey{}).(*turnBudget)
	return b
}
