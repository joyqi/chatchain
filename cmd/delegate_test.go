package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatchain/config"
	"chatchain/tool"

	"gopkg.in/yaml.v3"
)

type nopHTTP struct{}

func (nopHTTP) HTTPClient() *http.Client { return nil }

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

func agentsNode(t *testing.T, body string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return *doc.Content[0] // unwrap the document node
}

// The parallel decision has to survive a whole chain: a provider entry's
// toolset → readOnlyRegistry → AgentInfo.ReadOnly → the answer the tool gives
// for a call naming that agent. Every link was plausible alone, and nothing
// asserted they compose — which is exactly how a fan-out silently serializes.
func TestDelegateParallelFollowsTheAgentsToolset(t *testing.T) {
	cfg := loadConfig(t, `
providers:
  worker:  {type: openai, key: k, model: m}
  scout:   {type: openai, key: k, model: m, tools: {code: {read_only: true}}}
  codeboy: {type: openai, key: k, model: m, tools: {code: }}
`)
	node := agentsNode(t, `
agents:
  fast: {provider: worker, description: has no tools at all}
  scout: {provider: scout, description: searches but cannot write}
  slow: {provider: codeboy, description: can edit files}
`)
	del, err := buildDelegator(cfg, node, nopHTTP{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildDelegator: %v", err)
	}
	if info, ok := del.Agent("fast"); !ok || !info.ReadOnly {
		t.Error("an agent with no toolset must resolve as read-only")
	}
	// The case the whole feature was blocked on: an agent that can SEARCH and
	// still fan out. Before code's read_only, granting search granted writes,
	// so the only parallel-capable agent was one with no tools at all.
	if info, ok := del.Agent("scout"); !ok || !info.ReadOnly {
		t.Error("an agent with the read-only code set must be read-only")
	}
	if info, ok := del.Agent("slow"); !ok || info.ReadOnly {
		t.Error("an agent holding edit_file/write_file must not be read-only")
	}

	reg := tool.Build(tool.Env{ProjectRoot: t.TempDir(), Delegate: del},
		map[string]yaml.Node{"delegate": {}}, func(string, ...any) {})
	if !reg.SupportsParallel("delegate", map[string]any{"agent": "fast"}) {
		t.Error("a delegation to a read-only agent must be allowed to run concurrently")
	}
	if !reg.SupportsParallel("delegate", map[string]any{"agent": "scout"}) {
		t.Error("a searching-but-not-writing agent must be allowed to run concurrently")
	}
	if reg.SupportsParallel("delegate", map[string]any{"agent": "slow"}) {
		t.Error("a delegation to a write-capable agent must stay serialized")
	}
	// Unknown and absent both mean serial: the permissive answer is never the
	// one an unresolved name falls back to.
	if reg.SupportsParallel("delegate", map[string]any{"agent": "nope"}) ||
		reg.SupportsParallel("delegate", nil) {
		t.Error("an unresolved agent must default to serialized")
	}
}

// A referenced provider without a model cannot be asked to pick one, so it has
// to fail at startup rather than as a 400 mid-conversation.
func TestDelegateRequiresAModel(t *testing.T) {
	cfg := loadConfig(t, "providers:\n  worker: {type: openai, key: k}\n")
	node := agentsNode(t, "agents:\n  fast: worker\n")
	if _, err := buildDelegator(cfg, node, nopHTTP{}, t.TempDir()); err == nil {
		t.Fatal("an agent whose provider has no model: must be a startup error")
	}
}

// The bare-string form is the common one and must mean the same as the
// mapping form with only `provider:` set.
func TestAgentRefAcceptsBothForms(t *testing.T) {
	var got struct {
		Agents map[string]agentRef `yaml:"agents"`
	}
	if err := yaml.Unmarshal([]byte("agents:\n  a: worker\n  b: {provider: worker, description: d}\n"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agents["a"].Provider != "worker" || got.Agents["a"].Description != "" {
		t.Errorf("bare string form = %+v", got.Agents["a"])
	}
	if got.Agents["b"].Provider != "worker" || got.Agents["b"].Description != "d" {
		t.Errorf("mapping form = %+v", got.Agents["b"])
	}
}

// A child must not be handed the tool that made it: recursive delegation is
// unbounded in a way no per-run cap describes.
func TestChildToolsDropDelegate(t *testing.T) {
	raw := map[string]yaml.Node{"code": {}, "delegate": {}, "shell": {}}
	got := childTools(raw)
	if _, ok := got["delegate"]; ok {
		t.Error("the child kept the delegate set")
	}
	if len(got) != 2 {
		t.Errorf("childTools dropped more than delegate: %v", got)
	}
}

// AgentMode only injects the AGENTS.md/skills text; load_skill comes from the
// agent SET, which the main session enables separately. A child configured
// `agent: true` was told which skills exist and given no way to open one.
func TestDelegateChildGetsAgentModeTools(t *testing.T) {
	has := func(d tool.Dispatcher, name string) bool {
		for _, def := range d.Tools() {
			if def.Name == name {
				return true
			}
		}
		return false
	}
	on, warn := buildChildTools(t.TempDir(), nil, true)
	if len(warn) > 0 {
		t.Fatalf("unexpected warnings: %v", warn)
	}
	if !has(on, "load_skill") {
		t.Error("an agent-mode child must be able to open the skills it is told about")
	}
	off, _ := buildChildTools(t.TempDir(), nil, false)
	if has(off, "load_skill") {
		t.Error("a child without agent mode gained load_skill")
	}

	// And the flag reaches the builder from the provider entry: load_skill is
	// not parallel-safe, so an agent-mode child cannot be classified read-only.
	cfg := loadConfig(t, "providers:\n  skilled: {type: openai, key: k, model: m, agent: true}\n")
	del, err := buildDelegator(cfg, agentsNode(t, "agents:\n  a: skilled\n"), nopHTTP{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildDelegator: %v", err)
	}
	if info, _ := del.Agent("a"); info.ReadOnly {
		t.Error("an agent-mode child holds load_skill and cannot be read-only")
	}
}

// A malformed toolset in an agent's provider entry has to be a startup error.
// It was validated with a silent warnf and then rebuilt with a loud one per
// delegation, so it passed startup and later wrote to stderr mid-turn.
func TestDelegateRejectsAMalformedChildToolset(t *testing.T) {
	cfg := loadConfig(t, `
providers:
  broken: {type: openai, key: k, model: m, tools: {code: [not, a, mapping]}}
`)
	node := agentsNode(t, "agents:\n  a: broken\n")
	_, err := buildDelegator(cfg, node, nopHTTP{}, t.TempDir())
	if err == nil {
		t.Fatal("a malformed child toolset must fail at startup")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("the error should name the agent: %v", err)
	}
}
