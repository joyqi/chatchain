package chat

import (
	"context"
	"sync"

	"chatchain/provider"
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

// The run's delegation ledger: what everything this run delegated to cost,
// aggregated so the report can state it.
//
// It exists because the two accountings disagreed. turnBudget already counts
// a child's rounds against --max-turns — the run pays for them — while the
// JSON report counted only the parent's own API calls. A caller running this
// binary as a child was billed for rounds the report did not mention, and
// that caller is the consumer chat/output.go names first.
//
// The total stays SEPARATE from the parent's own usage rather than folded
// into it. They answer different questions — what this agent spent, and what
// it spent by delegating — and merging them produces the puzzle of two rounds
// costing twenty thousand tokens.
type delegationLedger struct {
	mu     sync.Mutex
	rounds int
	usage  TokenUsage
}

// add records one finished child. Called from parallel delegations, so it
// locks; a failed child counts too, because its rounds were billed.
func (l *delegationLedger) add(rounds int, u provider.Usage) {
	if l == nil || rounds == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rounds += rounds
	l.usage.add(u)
}

// report returns nil when nothing was delegated, so a run that delegated
// nothing carries no empty section.
func (l *delegationLedger) report() *DelegatedReport {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rounds == 0 {
		return nil
	}
	return &DelegatedReport{Rounds: l.rounds, Usage: l.usage}
}

type delegationLedgerKey struct{}

func withDelegationLedger(ctx context.Context, l *delegationLedger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, delegationLedgerKey{}, l)
}

func delegationLedgerFrom(ctx context.Context) *delegationLedger {
	l, _ := ctx.Value(delegationLedgerKey{}).(*delegationLedger)
	return l
}
