// Package tool provides chatchain's built-in toolsets — named groups of
// internal tools the user enables through the config file (alongside, or
// instead of, MCP servers).
//
// Each toolset is registered in the sets table by name. A provider's `tools:`
// config maps a set name to the set's shared raw config; the set factory
// decodes that one config instance and hands it to every tool it constructs
// (an empty value means defaults). Current sets: "shell" (bash), "code"
// (file tools), and "agent" (load_skill). The Registry aggregates the enabled
// tools behind the Dispatcher surface, and Merge combines several dispatchers
// (e.g. built-ins + an MCP manager) into one.
package tool

import (
	"context"
	"fmt"
	"sort"

	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// Dispatcher is the surface the chat loop uses to advertise tools to the model
// and execute the model's tool calls. Both *Registry and the MCP manager satisfy
// it; Merge combines several into one.
type Dispatcher interface {
	// Tools returns the tool definitions to advertise to the provider.
	Tools() []provider.ToolDef
	// CallTool executes a tool call. It returns the result text, whether the
	// call is an error (surfaced to the model), and a hard error (transport-level).
	CallTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error)
}

// Tool is a single built-in tool.
type Tool interface {
	Def() provider.ToolDef
	Call(ctx context.Context, args map[string]any) (text string, isError bool, err error)
}

// approver is an optional Tool interface: a tool whose calls change state on
// the user's machine reports that each call needs interactive user approval.
// A tool may report false when its set is configured to auto-approve.
type approver interface {
	RequiresApproval() bool
}

// ApprovalReporter is an optional Dispatcher capability: it reports whether a
// named tool's calls need interactive user approval before execution. The
// chat layer gates such calls behind a confirmation prompt, and rejects them
// outright in non-interactive runs.
type ApprovalReporter interface {
	RequiresApproval(name string) bool
}

// interactiveTool is an optional Tool interface: a tool whose call puts its
// own surface in front of the user (the ask set). Such calls have no
// execution to narrate — the chat layer keeps them out of the activity panel
// and routes the waiting state to the attention channels instead.
type interactiveTool interface {
	Interactive() bool
}

// InteractionReporter is an optional Dispatcher capability mirroring
// ApprovalReporter: it reports whether a named tool's calls are interactive
// (they open their own user-facing surface). Parts without the capability —
// e.g. the MCP manager — are never interactive.
type InteractionReporter interface {
	Interactive(name string) bool
}

// Env carries host context the toolsets need at construction time.
type Env struct {
	// ProjectRoot anchors project-scoped tools — the agent set's skill
	// discovery. Empty means "resolve from the working directory at call time".
	ProjectRoot string
	// Interact lets a tool put a question to the user through the host UI
	// (the ask set). nil in non-interactive runs — a set that needs it
	// returns no tools, so the model never sees what it cannot use.
	Interact Interactor
}

// Interactor is the host-side seam for model-initiated user interaction: one
// spec-driven entry covering single-select, multi-select, and confirm (a
// two-option single-select). The chat layer implements it over the tabbed
// surface engine; tools stay ignorant of the terminal stack.
type Interactor interface {
	Ask(ctx context.Context, spec AskSpec) (AskResult, error)
}

// AskSpec is one interaction: 1–4 questions answered on one surface (Tab
// switches, Enter commits all, ESC declines the whole ask).
type AskSpec struct {
	Questions []AskQuestion
}

// AskQuestion is a single question. Header is the SHORT tab label; Question
// is the body line. AllowCustom appends an "Other…" choice that opens a text
// field.
type AskQuestion struct {
	Header      string
	Question    string
	Options     []AskOption
	Multiple    bool
	AllowCustom bool
}

// AskOption is one selectable choice.
type AskOption struct {
	Label       string
	Description string
}

// AskAnswer is one question's outcome: the chosen option labels (one for
// single-select, any number for multi), or the user's typed text.
type AskAnswer struct {
	Selected []string
	Custom   string
}

// AskResult reports the interaction. Declined means the user dismissed the
// surface — a valid outcome the model should handle, not an error.
type AskResult struct {
	Declined bool
	Answers  []AskAnswer
}

// SetFactory builds a toolset's tools from the set's shared raw YAML config.
// A zero/null node means "use defaults" — every factory must succeed on it.
type SetFactory func(env Env, node yaml.Node) ([]Tool, error)

// sets is the central registry of built-in toolsets, one source file per set,
// named after it (shell.go, agent.go, code.go). Adding a set = a file with
// its factory and one line here; growing a set = its factory returns one more
// Tool. Future candidate: "web" (browse/search).
var sets = map[string]SetFactory{
	"shell": newShellSet,
	"agent": newAgentSet,
	"code":  newCodeSet,
	"ask":   newAskSet,
}

// SetDisabled reports an explicit boolean-false config value for a set —
// the opt-out for sets that are otherwise enabled by default (ask). A
// missing key is NOT disabled: presence-enables, false-disables, absence
// defers to the default policy.
func SetDisabled(raw map[string]yaml.Node, name string) bool {
	n, ok := raw[name]
	if !ok {
		return false
	}
	return nodeDisables(n)
}

func nodeDisables(n yaml.Node) bool {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		return false
	}
	switch n.Value {
	case "false", "no", "off":
		return true
	}
	return false
}

// Registry holds the enabled built-in tools and satisfies Dispatcher.
type Registry struct {
	order []Tool
	index map[string]Tool
}

// Build constructs the enabled toolsets from the per-set raw config: a key's
// presence enables that set. Unknown set names and construction errors are
// reported via warnf (nil to suppress) and skipped — a bad entry never aborts
// startup (mirrors MCP's graceful degradation). Keys are processed in sorted
// order so collisions resolve deterministically.
func Build(env Env, raw map[string]yaml.Node, warnf func(string, ...any)) *Registry {
	r := &Registry{index: make(map[string]Tool)}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if nodeDisables(raw[name]) {
			continue // explicit opt-out (e.g. ask: false)
		}
		f, ok := sets[name]
		if !ok {
			if warnf != nil {
				warnf("unknown toolset %q (ignored)", name)
			}
			continue
		}
		tools, err := f(env, raw[name])
		if err != nil {
			if warnf != nil {
				warnf("toolset %q: %v (ignored)", name, err)
			}
			continue
		}
		r.add(tools)
	}
	return r
}

// EnableSet registers the named toolset with its default (empty) config.
// Agent mode uses it to auto-register the agent set without a `tools:` entry;
// tools already enabled by the config keep their configured instance.
// Failures are reported via warnf (nil to suppress) and skipped, like Build.
func (r *Registry) EnableSet(env Env, name string, warnf func(string, ...any)) {
	f, ok := sets[name]
	if !ok {
		if warnf != nil {
			warnf("unknown toolset %q (ignored)", name)
		}
		return
	}
	tools, err := f(env, yaml.Node{})
	if err != nil {
		if warnf != nil {
			warnf("toolset %q: %v (ignored)", name, err)
		}
		return
	}
	r.add(tools)
}

// add appends tools, skipping any whose name is already registered (the
// earlier registration — e.g. a configured instance — wins).
func (r *Registry) add(tools []Tool) {
	for _, t := range tools {
		name := t.Def().Name
		if _, dup := r.index[name]; dup {
			continue
		}
		r.order = append(r.order, t)
		r.index[name] = t
	}
}

// Tools returns the definitions of the enabled built-in tools.
func (r *Registry) Tools() []provider.ToolDef {
	if r == nil {
		return nil
	}
	out := make([]provider.ToolDef, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, t.Def())
	}
	return out
}

// RequiresApproval reports whether the named built-in tool asks for
// interactive approval (via the optional approver interface).
func (r *Registry) RequiresApproval(name string) bool {
	if r == nil {
		return false
	}
	t, ok := r.index[name]
	if !ok {
		return false
	}
	a, ok := t.(approver)
	return ok && a.RequiresApproval()
}

// Interactive reports whether the named built-in tool runs its own user
// interaction (via the optional interactiveTool interface).
func (r *Registry) Interactive(name string) bool {
	if r == nil {
		return false
	}
	t, ok := r.index[name]
	if !ok {
		return false
	}
	i, ok := t.(interactiveTool)
	return ok && i.Interactive()
}

// CallTool dispatches a call to the matching built-in tool.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if r == nil {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}
	t, ok := r.index[name]
	if !ok {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}
	return t.Call(ctx, args)
}

// Merge combines several dispatchers behind one surface. Nil parts (including a
// typed-nil whose Tools() is nil-safe) contribute nothing. On a tool-name
// collision the earlier dispatcher wins, so pass built-ins before MCP to let
// them take precedence. The result is always non-nil.
//
// The merged view is LIVE, not a snapshot: Tools() and CallTool re-query each
// part on every call, so a part whose tool set grows after Merge (e.g. an MCP
// manager still connecting servers in the background) is reflected immediately.
func Merge(parts ...Dispatcher) Dispatcher {
	md := &multiDispatcher{}
	for _, p := range parts {
		if p != nil {
			md.parts = append(md.parts, p)
		}
	}
	return md
}

// multiDispatcher routes each tool call to whichever merged part owns the tool
// name, re-querying parts live so late-arriving tools appear without a rebuild.
type multiDispatcher struct {
	parts []Dispatcher
}

func (m *multiDispatcher) Tools() []provider.ToolDef {
	var tools []provider.ToolDef
	seen := make(map[string]bool)
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if seen[def.Name] {
				continue // earlier part wins the name
			}
			seen[def.Name] = true
			tools = append(tools, def)
		}
	}
	return tools
}

func (m *multiDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if def.Name == name {
				return p.CallTool(ctx, name, args) // first (earliest) owner
			}
		}
	}
	return "", true, fmt.Errorf("unknown tool: %s", name)
}

// RequiresApproval routes the question to the part owning the tool name.
// Parts without the ApprovalReporter capability (e.g. the MCP manager) never
// require approval — their behavior is unchanged.
func (m *multiDispatcher) RequiresApproval(name string) bool {
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if def.Name == name {
				ar, ok := p.(ApprovalReporter)
				return ok && ar.RequiresApproval(name)
			}
		}
	}
	return false
}

// Interactive routes the question to the part owning the tool name; parts
// without the InteractionReporter capability are never interactive.
func (m *multiDispatcher) Interactive(name string) bool {
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if def.Name == name {
				ir, ok := p.(InteractionReporter)
				return ok && ir.Interactive(name)
			}
		}
	}
	return false
}
