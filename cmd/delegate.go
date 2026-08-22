package cmd

import (
	"fmt"
	"net/http"
	"os"

	"chatchain/chat"
	"chatchain/config"
	"chatchain/provider"
	"chatchain/tool"

	"gopkg.in/yaml.v3"
)

// Wiring for the delegate toolset. Resolving an agent name to something
// runnable lives here because config → provider → toolset is what this
// package already does for the main session; the chat layer runs the child
// and the tool layer decides whether a delegation is allowed, and neither
// needs to learn how a provider entry is spelled.

// delegateConfig is `tools: delegate:`. An agent IS a provider entry — see
// tool/delegate.go for why nothing about the child is respecified here.
type delegateConfig struct {
	Agents   map[string]agentRef `yaml:"agents"`
	MaxTurns int                 `yaml:"max_turns"`
}

// agentRef is a provider name, optionally with the one thing the provider
// entry cannot carry: what this agent is FOR. The bare-string form is the
// common case and stays a bare string.
type agentRef struct {
	Provider    string `yaml:"provider"`
	Description string `yaml:"description"`
}

func (a *agentRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&a.Provider)
	}
	type raw agentRef // shed the custom unmarshaller to avoid recursing
	return n.Decode((*raw)(a))
}

// max_turns defaults to unlimited, like --max-turns and like the interactive
// loop, which states the reason at chat/run.go: the user is the brake.
//
// A child is no exception to that. ESC cancels the turn, and the context it
// cancels reaches the child's every round — so the brake works on a
// delegation exactly as it does on anything else. The earlier default of 30
// rested on the child having nobody watching it, which is not true.
//
// The key stays, because a cap the user chooses is different from one they
// were given. What it is NOT is a guard against a wedged child: that failure
// is measured in wall-clock, the way bash and pi's delegate extension measure
// it, and thirty cheap rounds cost nothing like three expensive ones.

// buildDelegator resolves every configured agent up front — a name that does
// not resolve is a startup error, not a surprise three tool calls into a
// conversation.
func buildDelegator(cfg *config.Config, node yaml.Node, hc httpClientSource, root string, warnf func(string, ...any)) (*chat.Delegator, error) {
	var sc delegateConfig
	if !node.IsZero() {
		if err := node.Decode(&sc); err != nil {
			return nil, fmt.Errorf("config must be a mapping (agents, max_turns): %w", err)
		}
	}
	if len(sc.Agents) == 0 {
		return nil, fmt.Errorf("no agents configured (add `agents:` mapping agent names to provider names)")
	}
	maxTurns := sc.MaxTurns
	if maxTurns < 0 {
		maxTurns = 0
	}

	type resolved struct {
		ptype string
		pc    config.ProviderConfig
		tools map[string]yaml.Node
	}
	agents := make(map[string]tool.AgentInfo, len(sc.Agents))
	byName := make(map[string]resolved, len(sc.Agents))

	for name, ref := range sc.Agents {
		if ref.Provider == "" {
			return nil, fmt.Errorf("agent %q: no provider named", name)
		}
		ptype, pc := cfg.Get(ref.Provider)
		if err := checkProviderName(cfg, ref.Provider, ptype); err != nil {
			return nil, fmt.Errorf("agent %q: %w", name, err)
		}
		// A child has no way to be asked which model to use: the main session
		// answers a missing `model:` with the interactive picker or by
		// demanding -M, and neither exists down here. Requiring it at startup
		// is the only place the answer can still be useful — otherwise the
		// empty name reaches the provider and comes back as a 400 in the
		// middle of somebody's conversation.
		if pc.Model == "" {
			return nil, fmt.Errorf("agent %q: provider %q has no `model:` (a delegated agent cannot be asked to pick one)", name, ref.Provider)
		}
		tools := childTools(pc.Tools)
		// A child's toolset is built once here so its access can be reported
		// to the model and, more importantly, so the parallel decision rests
		// on what the user configured rather than on what a task claims.
		reg := tool.Build(tool.Env{ProjectRoot: root}, tools, func(string, ...any) {})
		agents[name] = tool.AgentInfo{Description: ref.Description, ReadOnly: readOnlyRegistry(reg)}
		byName[name] = resolved{ptype: ptype, pc: pc, tools: tools}
	}

	build := func(name string) (chat.Child, error) {
		r, ok := byName[name]
		if !ok {
			return chat.Child{}, fmt.Errorf("unknown agent %q", name)
		}
		key := r.pc.Key
		if env := os.Getenv(providerEnvKey(r.ptype)); env != "" {
			key = env
		}
		if key == "" {
			return chat.Child{}, fmt.Errorf("agent %q: API key is required (set %s or `key:`)", name, providerEnvKey(r.ptype))
		}
		p, err := provider.New(r.ptype, key, r.pc.URL, r.pc.Model, r.pc.Temperature, hc.HTTPClient())
		if err != nil {
			return chat.Child{}, fmt.Errorf("agent %q: %w", name, err)
		}
		if r.pc.Effort != "" {
			if tun, ok := p.(provider.Tunable); ok {
				tun.SetEffort(r.pc.Effort)
			}
		}
		if r.pc.TopP != nil {
			if tun, ok := p.(provider.TopPTunable); ok {
				tun.SetTopP(r.pc.TopP)
			}
		}
		sys, err := r.pc.ResolveSystem()
		if err != nil {
			return chat.Child{}, fmt.Errorf("agent %q: %w", name, err)
		}
		// The child's own toolset: no ask seam (nobody to question but the
		// parent's user, and the child is not the conversation they are in)
		// and no Delegate, which is what stops the recursion.
		env := tool.Env{ProjectRoot: root}
		return chat.Child{
			Provider:  p,
			Dispatch:  tool.Build(env, r.tools, warnf),
			System:    sys,
			AgentMode: chat.AgentOptions{Enabled: r.pc.Agent, Root: root},
			MaxTurns:  maxTurns,
		}, nil
	}
	return chat.NewDelegator(agents, build), nil
}

// httpClientSource is the recording transport, narrowed to the one method a
// child needs — its requests belong in /debug alongside the parent's.
type httpClientSource interface{ HTTPClient() *http.Client }

// childTools strips the delegate set from a child's toolset. A child that
// could delegate would delegate recursively, and the cost of that is
// unbounded in a way no per-run cap describes. pi's delegate extension
// disables it in the child for the same reason.
func childTools(raw map[string]yaml.Node) map[string]yaml.Node {
	out := make(map[string]yaml.Node, len(raw))
	for k, v := range raw {
		if k == "delegate" {
			continue
		}
		out[k] = v
	}
	return out
}

// readOnlyRegistry reports whether every tool in the set may run
// concurrently, which is the same question as whether the set changes
// anything: the parallel opt-in already means "does not write, needs no
// approval, opens no surface". Reusing it keeps one definition of harmless
// instead of two that could disagree — and an empty set is trivially read-only.
func readOnlyRegistry(reg *tool.Registry) bool {
	for _, def := range reg.Tools() {
		if !reg.SupportsParallel(def.Name, nil) {
			return false
		}
	}
	return true
}
