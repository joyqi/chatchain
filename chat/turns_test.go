package chat

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/joyqi/iota/provider"
)

func TestTurnBudgetUnlimitedWithoutAFlag(t *testing.T) {
	// A cap nobody chose is not invented: no --max-turns means no budget.
	for _, n := range []int{0, -1} {
		if b := newTurnBudget(n); b != nil {
			t.Errorf("newTurnBudget(%d) = %+v, want nil (unlimited)", n, b)
		}
	}
	var nilBudget *turnBudget
	for i := 0; i < 1000; i++ {
		if !nilBudget.take() {
			t.Fatal("a nil budget must grant every round")
		}
	}
}

func TestTurnBudgetSpendsExactlyItsCap(t *testing.T) {
	b := newTurnBudget(3)
	for i := 0; i < 3; i++ {
		if !b.take() {
			t.Fatalf("round %d refused inside the cap", i+1)
		}
	}
	if b.take() {
		t.Error("the budget granted a fourth round")
	}
	if b.cap() != 3 {
		t.Errorf("cap() = %d, want the number the user wrote", b.cap())
	}
}

// Parallel delegations draw on this pool from several goroutines at once, so
// the count has to be exact under contention — a lost decrement is a cap that
// quietly overspends.
func TestTurnBudgetIsExactUnderContention(t *testing.T) {
	const cap = 50
	b := newTurnBudget(cap)
	var granted int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if b.take() {
					mu.Lock()
					granted++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if granted != cap {
		t.Errorf("granted %d rounds against a cap of %d", granted, cap)
	}
}

// The point of the change: the budget belongs to the RUN. Two loops sharing
// one pool stop at the total between them, not at the total each.
func TestTurnBudgetIsSharedAcrossLoops(t *testing.T) {
	budget := newTurnBudget(5)
	spend := func() int {
		tp := &loopingToolProvider{}
		history := []provider.Message{{Role: "user", Content: "go"}}
		_, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history,
			noopDispatcher{}.Tools(), "", 0, quietHost{rec: newRunRecorder(), turns: budget})
		if !errors.Is(err, errToolRoundsExceeded) {
			t.Fatalf("loop ended with %v, want the budget to stop it", err)
		}
		return tp.calls
	}
	first := spend()
	second := spend()
	if first != 5 {
		t.Errorf("first loop ran %d rounds, want the whole budget", first)
	}
	if second != 0 {
		t.Errorf("second loop ran %d rounds after the pool was spent, want 0", second)
	}
}

// A child recovers the pool from the context it was called with; an
// interactive run publishes none and every round is granted.
func TestTurnBudgetTravelsByContext(t *testing.T) {
	if got := turnBudgetFrom(context.Background()); got != nil {
		t.Errorf("a context with no budget yielded %+v, want nil", got)
	}
	b := newTurnBudget(2)
	ctx := withTurnBudget(context.Background(), b)
	if turnBudgetFrom(ctx) != b {
		t.Error("the budget did not survive the context")
	}
	// A nil budget must not put an unlimited marker in the context either.
	if turnBudgetFrom(withTurnBudget(context.Background(), nil)) != nil {
		t.Error("an absent budget was published as present")
	}
}

// --max-turns already charges the run for a child's rounds; the report has to
// charge it for their tokens. A caller running this binary as a child was
// billed for rounds the report never mentioned.
func TestDelegationLedgerAggregates(t *testing.T) {
	var l *delegationLedger
	if l.report() != nil {
		t.Error("a nil ledger reported something")
	}
	l = &delegationLedger{}
	if l.report() != nil {
		t.Error("a run that delegated nothing carries an empty section")
	}
	l.add(3, provider.Usage{Input: 900, Output: 100, Total: 1000})
	l.add(2, provider.Usage{Input: 400, Output: 50, Total: 450})
	rep := l.report()
	if rep == nil || rep.Rounds != 5 {
		t.Fatalf("report = %+v, want 5 rounds", rep)
	}
	if rep.Usage.TotalTokens != 1450 || rep.Usage.InputTokens != 1300 {
		t.Errorf("usage = %+v, want the sum of both children", rep.Usage)
	}
	// A child that never reached the provider adds nothing.
	l.add(0, provider.Usage{Input: 999})
	if l.report().Rounds != 5 || l.report().Usage.InputTokens != 1300 {
		t.Error("a child with no rounds moved the totals")
	}
}

// Parallel delegations finish on their own goroutines and land here at once.
func TestDelegationLedgerIsExactUnderContention(t *testing.T) {
	l := &delegationLedger{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.add(1, provider.Usage{Input: 2, Total: 3})
			}
		}()
	}
	wg.Wait()
	rep := l.report()
	if rep.Rounds != 400 || rep.Usage.TotalTokens != 1200 || rep.Usage.InputTokens != 800 {
		t.Errorf("ledger = %+v after 400 concurrent adds", rep)
	}
}

// The delegated total stays out of the parent's own figures: one says what
// this agent spent, the other what it spent by delegating.
func TestReportKeepsDelegatedSeparate(t *testing.T) {
	rec := newRunRecorder()
	rec.total.add(provider.Usage{Input: 9, Output: 1, Total: 10})
	rec.rounds = []RoundReport{{Round: 1}}
	rec.delegated = &delegationLedger{}
	rec.delegated.add(4, provider.Usage{Input: 3600, Output: 400, Total: 4000})

	rep := rec.report(&reportingProvider{}, "done", nil, nil, nil)
	if rep.Usage.TotalTokens != 10 {
		t.Errorf("own usage = %d, want the parent's own calls only", rep.Usage.TotalTokens)
	}
	if rep.Rounds != 1 {
		t.Errorf("own rounds = %d, want the parent's own rounds only", rep.Rounds)
	}
	if rep.Delegated == nil || rep.Delegated.Rounds != 4 || rep.Delegated.Usage.TotalTokens != 4000 {
		t.Errorf("delegated = %+v, want the children's total", rep.Delegated)
	}
	// And it is omitted entirely when nothing was delegated.
	bare := newRunRecorder()
	bare.delegated = &delegationLedger{}
	if bare.report(&reportingProvider{}, "x", nil, nil, nil).Delegated != nil {
		t.Error("a run that delegated nothing carries a delegated section")
	}
}
