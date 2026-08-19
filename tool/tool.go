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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

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

// Presentation classifies how a tool call's lifecycle is displayed.
type Presentation uint8

const (
	// PresentGroup folds the call into the activity panel — the default for
	// plumbing calls (reads, searches, shell commands, MCP tools).
	PresentGroup Presentation = iota
	// PresentSurface marks a call that puts its own surface in front of the
	// user (the ask set): nothing to narrate — the chat layer routes the
	// waiting state to the attention channels instead.
	PresentSurface
	// PresentExpanded marks a call whose outcome deserves a standalone,
	// expanded block — file mutations showing their diff. Such calls close
	// the current activity group like a content boundary does.
	PresentExpanded
)

// presenter is an optional Tool interface declaring a non-default
// presentation for the tool's calls.
type presenter interface {
	Presentation() Presentation
}

// PresentationReporter is an optional Dispatcher capability mirroring
// ApprovalReporter: it reports the named tool's presentation class. Parts
// without the capability — e.g. the MCP manager — present as PresentGroup.
type PresentationReporter interface {
	Presentation(name string) Presentation
}

// headliner is an optional Tool interface: the tool writes the summary that
// follows its name in the call header, instead of the generic argument
// digest. The brackets, the name, and the styling stay with the chat layer —
// a tool only says what is worth showing about THIS call.
//
// Implementing it takes over completely: an empty summary renders as a bare
// "[name]", it does NOT fall back. That matters for tools whose arguments
// must never reach the header — edit_file's new_string is a whole file's
// worth of code.
type headliner interface {
	HeaderSummary(args map[string]any) string
}

// HeaderReporter is the Dispatcher-side mirror of headliner (as
// PresentationReporter mirrors approver). The bool separates the three states
// a bare string cannot: a summary, a deliberately empty summary, and "this
// tool has none — use the default digest".
type HeaderReporter interface {
	HeaderSummary(name string, args map[string]any) (string, bool)
}

// Owner is an optional Dispatcher capability: name ownership beyond the
// currently ADVERTISED tools. A deferring wrapper hides tools from Tools()
// but still owns their names — Merge routes calls (and capability queries)
// through Owns, so a direct call to a hidden tool reaches the wrapper's
// implicit-load path instead of dying as "unknown tool".
type Owner interface {
	Owns(name string) bool
}

// Artifact is a call's display payload, posted through the context side
// channel: content meant for the USER's eyes only (a unified diff), kept out
// of the model-facing result text so it never costs tokens. Lines are
// unstyled; the chat layer renders them by Kind.
type Artifact struct {
	Kind  string   // "diff"
	Title string   // e.g. the file's display path
	Lines []string // hunk lines for "diff" (@@ headers, +/-/context rows)
}

// artifactKey carries the per-call artifact slot in the context.
type artifactKey struct{}

type artifactSlot struct {
	mu  sync.Mutex
	art *Artifact
}

// WithArtifact injects an artifact slot into ctx ahead of a CallTool and
// returns the collector; the last artifact the call posts wins.
func WithArtifact(ctx context.Context) (context.Context, func() *Artifact) {
	slot := &artifactSlot{}
	return context.WithValue(ctx, artifactKey{}, slot), func() *Artifact {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		return slot.art
	}
}

// PostArtifact records the call's display payload; a no-op when the host did
// not inject a slot (non-interactive runs, tests).
func PostArtifact(ctx context.Context, art Artifact) {
	slot, ok := ctx.Value(artifactKey{}).(*artifactSlot)
	if !ok {
		return
	}
	slot.mu.Lock()
	slot.art = &art
	slot.mu.Unlock()
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

// Root is the toolsets' anchor directory: the configured project root, else
// the working directory, always absolute. Three toolsets had byte-identical
// copies of this before — and one of them had drifted, skipping the Abs step.
func (e Env) Root() string {
	root := e.ProjectRoot
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return root
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

// Presentation reports the named built-in tool's presentation class (via the
// optional presenter interface; default PresentGroup).
func (r *Registry) Presentation(name string) Presentation {
	if r == nil {
		return PresentGroup
	}
	t, ok := r.index[name]
	if !ok {
		return PresentGroup
	}
	if p, ok := t.(presenter); ok {
		return p.Presentation()
	}
	return PresentGroup
}

// HeaderSummary reports the named built-in tool's own call summary (via the
// optional headliner interface). ok=false means the tool has none and the
// caller should fall back to the generic argument digest.
func (r *Registry) HeaderSummary(name string, args map[string]any) (string, bool) {
	if r == nil {
		return "", false
	}
	t, ok := r.index[name]
	if !ok {
		return "", false
	}
	h, ok := t.(headliner)
	if !ok {
		return "", false
	}
	return h.HeaderSummary(args), true
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

// owner finds the first part owning the name: by the Owner capability when
// implemented (which sees hidden/deferred names), else by the advertised
// tool list.
func (m *multiDispatcher) owner(name string) Dispatcher {
	for _, p := range m.parts {
		if o, ok := p.(Owner); ok {
			if o.Owns(name) {
				return p
			}
			continue
		}
		for _, def := range p.Tools() {
			if def.Name == name {
				return p
			}
		}
	}
	return nil
}

func (m *multiDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if p := m.owner(name); p != nil {
		return p.CallTool(ctx, name, args)
	}
	return "", true, fmt.Errorf("unknown tool: %s", name)
}

// RequiresApproval routes the question to the part owning the tool name.
// Parts without the ApprovalReporter capability (e.g. the MCP manager) never
// require approval — their behavior is unchanged.
func (m *multiDispatcher) RequiresApproval(name string) bool {
	if p := m.owner(name); p != nil {
		ar, ok := p.(ApprovalReporter)
		return ok && ar.RequiresApproval(name)
	}
	return false
}

// Presentation routes the question to the part owning the tool name; parts
// without the PresentationReporter capability present as PresentGroup.
func (m *multiDispatcher) Presentation(name string) Presentation {
	if p := m.owner(name); p != nil {
		if pr, ok := p.(PresentationReporter); ok {
			return pr.Presentation(name)
		}
	}
	return PresentGroup
}

// HeaderSummary routes the question to the part owning the tool name; parts
// without the HeaderReporter capability (the MCP manager) report none, and
// their calls keep the default argument digest.
func (m *multiDispatcher) HeaderSummary(name string, args map[string]any) (string, bool) {
	if p := m.owner(name); p != nil {
		if hr, ok := p.(HeaderReporter); ok {
			return hr.HeaderSummary(name, args)
		}
	}
	return "", false
}

// SearchTools forwards the client-executed search to the first part with
// the capability (the protocol defer modes' wrapper).
func (m *multiDispatcher) SearchTools(query string) []provider.ToolDef {
	for _, p := range m.parts {
		if ts, ok := p.(ToolSearcher); ok {
			return ts.SearchTools(query)
		}
	}
	return nil
}

// DeferredTools aggregates deferred-tool status from every implementing part.
func (m *multiDispatcher) DeferredTools() []DeferredToolStatus {
	var out []DeferredToolStatus
	for _, p := range m.parts {
		if di, ok := p.(DeferInspector); ok {
			out = append(out, di.DeferredTools()...)
		}
	}
	return out
}

// TakePendingLoads drains frozen-mount loads from every implementing part.
func (m *multiDispatcher) TakePendingLoads() []provider.ToolDef {
	var out []provider.ToolDef
	for _, p := range m.parts {
		if pl, ok := p.(PendingLoader); ok {
			out = append(out, pl.TakePendingLoads()...)
		}
	}
	return out
}

// boolArg reads an optional boolean tool argument. Models occasionally
// serialize booleans as strings; a silent type mismatch must not flip a
// flag's meaning (a promised multi-select rendering single-select, a
// replace_all editing one occurrence), so string spellings coerce.
func boolArg(m map[string]any, key string, def bool) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
