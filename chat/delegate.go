package chat

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/joyqi/iota/provider"
	"github.com/joyqi/iota/tool"
)

// Delegation: running a child agent from inside a parent's tool call.
//
// The child is this same machinery one level down — a provider, a toolset and
// runOnce — which is why there is so little here. What this file owns is the
// three things that only make sense at the boundary: resolving an agent name
// to something runnable, forwarding the child's approval questions to the one
// user who exists, and refusing to let the child delegate in turn.

// quietHost is what the non-interactive tool loop needs from whoever started
// it: somewhere to record what each round cost, and — when a user exists
// somewhere up the stack — a way to put an approval question to them.
type quietHost struct {
	rec *runRecorder
	// turns is the RUN's budget, shared with every delegated child. The
	// recorder beside it is deliberately NOT shared: what a child cost is
	// reported per child, while what the run may spend is one pool.
	turns *turnBudget
	// approve, when set, forwards a state-changing call to whoever owns the
	// terminal. A -m run leaves it nil because there is nobody to ask; a
	// delegated child sets it because it has no user of its own but runs
	// inside a parent that does.
	approve func(ctx context.Context, tc provider.ToolCall, detail string) (bool, string)
}

// askApproval resolves one gated call: allowed, or the refusal to hand back
// as the call's result.
//
// detail is what the call is ABOUT — the path, the command — rendered by the
// caller, because only it holds the dispatcher that owns the tool. A child's
// tools are its own, and the parent asking on its behalf cannot describe them.
func (h quietHost) askApproval(ctx context.Context, tc provider.ToolCall, detail string) (bool, string) {
	if h.approve == nil {
		return false, fmt.Sprintf("%s was not executed: it requires interactive approval, "+
			"which is unavailable in this non-interactive run. Set the toolset's auto-approve option "+
			"(tools.code.auto_write / tools.shell.auto_run) to permit it here.", tc.Name)
	}
	return h.approve(ctx, tc, detail)
}

// Child is everything a delegated run needs, assembled by the host.
//
// The host builds it because config → provider → dispatcher is exactly what
// it already does for the main session. A second implementation here is how
// the two would come to disagree about which knobs a provider entry sets.
type Child struct {
	Provider provider.Provider
	Dispatch tool.Dispatcher
	System   string
	// AgentMode is the AGENTS.md/skills overlay setting — agent MODE, not
	// the delegated agent. The two senses of the word meet in this struct,
	// so this one says which it is.
	AgentMode AgentOptions
	MaxTurns  int
}

// ChildFactory builds a child for a configured agent name.
type ChildFactory func(agent string) (Child, error)

// Delegator runs child agents for the delegate toolset (tool.Delegator).
type Delegator struct {
	agents map[string]tool.AgentInfo
	names  []string
	build  ChildFactory

	mu      sync.Mutex
	approve func(ctx context.Context, agent string, tc provider.ToolCall, detail string) (bool, string)
}

// NewDelegator prepares the seam. Names are sorted once: the agent list
// reaches the model as a schema enum, and a set that reshuffled between runs
// would defeat prompt caching for no reason.
func NewDelegator(agents map[string]tool.AgentInfo, build ChildFactory) *Delegator {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return &Delegator{agents: agents, names: names, build: build}
}

func (d *Delegator) AgentNames() []string { return d.names }

func (d *Delegator) Agent(name string) (tool.AgentInfo, bool) {
	info, ok := d.agents[name]
	return info, ok
}

// SetApprover binds the parent's approval gate, which only exists once there
// is a live UI. Until then a child's state-changing calls are refused, which
// is the same answer any other non-interactive run gives.
func (d *Delegator) SetApprover(fn func(ctx context.Context, agent string, tc provider.ToolCall, detail string) (bool, string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approve = fn
}

func (d *Delegator) approver() func(context.Context, string, provider.ToolCall, string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.approve
}

// Run executes one delegation to completion.
func (d *Delegator) Run(ctx context.Context, spec tool.DelegateSpec) (tool.DelegateResult, error) {
	if _, ok := d.agents[spec.Agent]; !ok {
		return tool.DelegateResult{}, fmt.Errorf("unknown agent %q", spec.Agent)
	}
	child, err := d.build(spec.Agent)
	if err != nil {
		return tool.DelegateResult{}, err
	}
	// The per-task effort override lands on a provider built for this call
	// alone, so it cannot leak into the parent's or another child's sampling.
	if spec.Effort != "" {
		if tun, ok := child.Provider.(provider.Tunable); ok {
			tun.SetEffort(spec.Effort)
		}
	}

	host := quietHost{rec: newRunRecorder(), turns: turnBudgetFrom(ctx)}
	if fn := d.approver(); fn != nil {
		// Only one child can be waiting on the user at a time — the terminal
		// is single-threaded even when the delegations are not. In practice
		// concurrent delegations never reach here at all, because a child may
		// only run in parallel when its agent grants no state-changing tool
		// (tool.delegateTool.SupportsParallel); the lock is what keeps that
		// from being load-bearing.
		host.approve = func(ctx context.Context, tc provider.ToolCall, detail string) (bool, string) {
			d.mu.Lock()
			defer d.mu.Unlock()
			return fn(ctx, spec.Agent, tc, detail)
		}
	}

	started := time.Now()
	reply, _, _, err := runOnce(ctx, child.Provider, spec.Task, child.System,
		child.Dispatch, child.AgentMode, child.MaxTurns, host)
	res := tool.DelegateResult{
		Reply:    reply,
		Rounds:   len(host.rec.rounds),
		Usage:    host.rec.usage(),
		Duration: time.Since(started),
	}
	// The run's own report has to account for this: --max-turns already
	// charges the run for a child's rounds, and the tokens they cost belong
	// to the same run. A failed child counts — its rounds were billed.
	delegationLedgerFrom(ctx).add(res.Rounds, res.Usage)
	return res, err
}
