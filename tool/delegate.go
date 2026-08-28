package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/joyqi/iota/internal/timefmt"
	"github.com/joyqi/iota/internal/tokfmt"
	"github.com/joyqi/iota/provider"

	"gopkg.in/yaml.v3"
)

// The "delegate" set: one tool, delegate, which runs a configured child agent
// on a task and returns its answer.
//
// The child is a full agent — its own context, its own tool loop — and what
// comes back is its final reply ALONE. No tool calls, no reasoning. That is
// the whole point: a search that takes twenty rounds costs the parent one
// paragraph. Claude Code, Codex and pi all landed on the same contract.
//
// An agent is named in config and IS a provider entry:
//
//	tools:
//	  delegate:
//	    agents:
//	      search: fast-provider
//	      review:
//	        provider: careful-provider
//	        description: Reads a diff and reports what is wrong with it
//
// Nothing about the child is configured here a second time. Which model, which
// tools, whether those tools ask for approval, the system prompt, temperature,
// context window — the referenced provider entry already says all of it, and
// saying it twice is how the two copies would come to disagree. The only
// field that is not already over there is `description`, which is about the
// ROLE rather than the provider, and is the one thing the model has to go on
// when choosing.

func newDelegateSet(env Env, _ yaml.Node) ([]Tool, error) {
	// Same contract as the ask set: without the host seam the set
	// contributes no tools, so the model never sees what it cannot use.
	if env.Delegate == nil {
		return nil, nil
	}
	names := env.Delegate.AgentNames()
	if len(names) == 0 {
		return nil, fmt.Errorf("no agents configured (add `agents:` mapping agent names to provider names)")
	}
	return []Tool{&delegateTool{d: env.Delegate, names: names}}, nil
}

type delegateTool struct {
	d     Delegator
	names []string // configured agents, stable order
}

// SupportsParallel: two delegations may overlap only when neither child can
// change state, and that is a property of the AGENT this call names — not of
// the delegate tool, whose calls are all named the same.
//
// The answer is a lookup into the user's configuration, never an inference
// from the task text. A model that wanted a write-capable child to run
// concurrently could otherwise get one by describing its task as read-only.
// An unconfigured or missing agent resolves to false: unknown means serial.
func (t *delegateTool) SupportsParallel(args map[string]any) bool {
	info, ok := t.d.Agent(agentArg(args))
	return ok && info.ReadOnly
}

// agentArg reads the agent name the way every reader of it must. There are
// three — the parallel classification, the execution, and the call header —
// and they resolved it differently once, which let a call be admitted to a
// batch as one agent and then run as another. Whatever the rule is, all of
// them have to share it.
func agentArg(args map[string]any) string {
	return strings.TrimSpace(stringArg(args, "agent"))
}

// HeaderSummary puts the agent and the head of its brief in the call header —
// "[delegate search: every call site of parallelRun]". The task is the only
// thing that distinguishes two delegations to one agent, and the agent name
// is what says how much the call is about to cost.
func (t *delegateTool) HeaderSummary(args map[string]any) string {
	agent := agentArg(args) // the same reading the classifier and the run use
	task, _ := args["task"].(string)
	task = headerCommand(strings.TrimSpace(task))
	switch {
	case agent == "":
		return task
	case task == "":
		return agent
	}
	return agent + ": " + task
}

func (t *delegateTool) Def() provider.ToolDef {
	var b strings.Builder
	b.WriteString("Delegate a task to a child agent and get back its answer.\n\n" +
		"The child starts with NO knowledge of this conversation and reports back only its " +
		"final answer, so `task` must be self-contained — state the goal, the context it needs, " +
		"and what shape the answer should take. Prefer delegating work whose intermediate steps " +
		"you do not need to see (surveying a codebase, checking a hypothesis across many files); " +
		"doing it yourself is better when you need to watch it happen.\n\n" +
		"Available agents:\n")
	for _, name := range t.names {
		info, _ := t.d.Agent(name)
		desc := info.Description
		if desc == "" {
			desc = "(no description configured)"
		}
		access := "read-only"
		if !info.ReadOnly {
			access = "can modify files"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", name, access, desc)
	}

	return provider.ToolDef{
		Name:        "delegate",
		Description: strings.TrimRight(b.String(), "\n"),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{
					"type":        "string",
					"enum":        toAny(t.names),
					"description": "Which configured agent runs the task.",
				},
				"task": map[string]any{
					"type": "string",
					"description": "The complete brief. The child sees this and nothing else — " +
						"no history, no files you have already read.",
				},
				"effort": map[string]any{
					"type":        "string",
					"enum":        []any{"low", "medium", "high", "xhigh", "max"},
					"description": "Optional reasoning effort override for this one task.",
				},
			},
			"required": []any{"agent", "task"},
		},
	}
}

func (t *delegateTool) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	agent := agentArg(args)
	if agent == "" {
		return "missing required argument: agent", true, nil
	}
	if _, ok := t.d.Agent(agent); !ok {
		return fmt.Sprintf("unknown agent %q — configured agents: %s",
			agent, strings.Join(t.names, ", ")), true, nil
	}
	task := strings.TrimSpace(stringArg(args, "task"))
	if task == "" {
		return "missing required argument: task", true, nil
	}
	effort := strings.TrimSpace(stringArg(args, "effort"))
	if effort != "" && !provider.ValidEffort(effort) {
		return fmt.Sprintf("invalid effort %q: want low|medium|high|xhigh|max", effort), true, nil
	}

	res, err := t.d.Run(ctx, DelegateSpec{Agent: agent, Task: task, Effort: effort})
	// What the child cost goes to the USER through the artifact channel, not
	// into the result. Reporting a delegation's price by spending tokens on
	// the number would be its own small joke; this way the parent's
	// transcript shows it and the parent's context never carries it.
	//
	// A FAILED child is billed for the rounds it completed, and it is the run
	// worth investigating — reporting the cost only on success would hide it
	// exactly where it is surprising.
	if res.Rounds > 0 {
		PostArtifact(ctx, Artifact{Kind: "note", Lines: []string{
			fmt.Sprintf("%d round%s", res.Rounds, plural(res.Rounds)),
			tokfmt.Tokens(res.Usage.ContextTokens()) + " tokens",
			timefmt.Elapsed(res.Duration),
		}})
	}
	if err != nil {
		// The child's failure is the parent's result, not the parent's crash:
		// a returned error would abort the whole round, while a tool error
		// lets the model try something else.
		return fmt.Sprintf("delegation to %q failed: %v", agent, err), true, nil
	}
	if strings.TrimSpace(res.Reply) == "" {
		return fmt.Sprintf("agent %q finished without an answer after %d round(s)", agent, res.Rounds), true, nil
	}
	return res.Reply, false, nil
}

// stringArg reads a string argument, tolerating absence.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

// plural is the "s" a count needs; a header that says "1 rounds" reads as a
// bug in the thing that printed it.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
