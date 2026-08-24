package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeDelegator answers from a fixed table and records what Run was asked for.
type fakeDelegator struct {
	agents map[string]AgentInfo
	ran    []string
	res    DelegateResult
	err    error
}

func (f *fakeDelegator) AgentNames() []string {
	names := make([]string, 0, len(f.agents))
	for n := range f.agents {
		names = append(names, n)
	}
	return names
}
func (f *fakeDelegator) Agent(name string) (AgentInfo, bool) {
	info, ok := f.agents[name]
	return info, ok
}
func (f *fakeDelegator) Run(_ context.Context, spec DelegateSpec) (DelegateResult, error) {
	f.ran = append(f.ran, spec.Agent)
	return f.res, f.err
}

// The parallel classification and the execution must resolve "which agent" the
// same way. They did not: one read the raw argument and the other trimmed it,
// so a call could be admitted to a batch as a read-only agent and then run as
// a write-capable one.
func TestDelegateResolvesTheAgentOnceForBothPaths(t *testing.T) {
	d := &fakeDelegator{agents: map[string]AgentInfo{
		"reader": {ReadOnly: true},
		"writer": {ReadOnly: false},
	}, res: DelegateResult{Reply: "done", Rounds: 1}}
	tl := &delegateTool{d: d, names: []string{"reader", "writer"}}

	for _, spelling := range []string{"writer", " writer", "writer ", "  writer  "} {
		args := map[string]any{"agent": spelling, "task": "t"}
		if tl.SupportsParallel(args) {
			t.Errorf("agent %q classified as parallel-safe", spelling)
		}
		d.ran = nil
		if _, isErr, _ := tl.Call(context.Background(), args); isErr {
			t.Errorf("agent %q was rejected by Call but accepted by the classifier", spelling)
		}
		if len(d.ran) != 1 || d.ran[0] != "writer" {
			t.Errorf("agent %q ran %v, want the write-capable agent", spelling, d.ran)
		}
	}
	// And the read-only spelling stays parallel-safe with the same padding.
	if !tl.SupportsParallel(map[string]any{"agent": " reader "}) {
		t.Error("a padded read-only agent must still be parallel-safe")
	}
}

// A failed child is billed for the rounds it completed, and it is the run
// worth investigating — the accounting must not be the thing that goes
// missing exactly when the cost is surprising.
func TestDelegateReportsCostOfAFailedChild(t *testing.T) {
	d := &fakeDelegator{
		agents: map[string]AgentInfo{"a": {}},
		res:    DelegateResult{Rounds: 3, Duration: 2 * time.Second},
		err:    errors.New("upstream exploded"),
	}
	tl := &delegateTool{d: d, names: []string{"a"}}

	ctx, collect := WithArtifact(context.Background())
	text, isErr, err := tl.Call(ctx, map[string]any{"agent": "a", "task": "t"})
	if err != nil {
		t.Fatalf("a child's failure must be the parent's result, not its error: %v", err)
	}
	if !isErr || !strings.Contains(text, "upstream exploded") {
		t.Errorf("Call = (%q, %v), want the failure as an error result", text, isErr)
	}
	art := collect()
	if art == nil || art.Kind != "note" {
		t.Fatalf("a failed delegation posted no accounting: %+v", art)
	}
	if joined := strings.Join(art.Lines, " "); !strings.Contains(joined, "3 rounds") {
		t.Errorf("accounting = %q, want the rounds that were billed", joined)
	}
}

// A child that never reached the provider has nothing to account for, and a
// "0 rounds · 0 tokens" row would be noise dressed as information.
func TestDelegateSkipsAccountingWhenNothingRan(t *testing.T) {
	d := &fakeDelegator{agents: map[string]AgentInfo{"a": {}}, err: errors.New("no api key")}
	tl := &delegateTool{d: d, names: []string{"a"}}
	ctx, collect := WithArtifact(context.Background())
	if _, isErr, _ := tl.Call(ctx, map[string]any{"agent": "a", "task": "t"}); !isErr {
		t.Error("a build failure must be an error result")
	}
	if art := collect(); art != nil {
		t.Errorf("posted accounting for a child that never ran: %+v", art)
	}
}
