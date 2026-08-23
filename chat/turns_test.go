package chat

import (
	"context"
	"errors"
	"sync"
	"testing"

	"chatchain/provider"
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
