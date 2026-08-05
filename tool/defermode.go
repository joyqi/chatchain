package tool

// The defer MODE decides which protocol realizes deferred tool loading for a
// provider — orthogonal to WHICH servers defer (the per-server `defer` key).
// Every mode is a strategy behind the DeferMode interface, registered in the
// deferModes table (one entry per mode, mirroring the toolsets table):
// adding a mode = one implementation + one line here.
//
// Planned modes beyond normal, from the protocol survey (docs/design/
// tool-defer.md): "reference" (Anthropic defer_loading — schemas expand
// server-side as tool_reference blocks, prefix cache preserved),
// "tool-search" (OpenAI Responses tool_search with client execution — our
// search logic feeds tool_search_output items), and "system-tools" (Kimi
// K3's dynamically-loaded tools: rides the OpenAI-compatible chat
// completions dialect against Moonshot directly — a system-role message
// carrying a `tools` field and NO `content` (400 otherwise), APPENDED only
// (inserting busts their prefix cache), kimi-k3 only — other models fail
// server-side with "tokenization failed").
// Their provider-side glue will arrive as optional capability interfaces on
// the dispatcher the mode builds (e.g. a SearchTools seam the llm layer
// discovers) — the Wrap seam below stays sufficient.

// DeferMode realizes deferred loading over one strategy.
type DeferMode interface {
	// Name is the config value selecting this mode.
	Name() string
	// Supports reports whether the mode can run against a provider type.
	// The check is necessary but not sufficient — model-level requirements
	// (e.g. a minimum model version) cannot be judged client-side, so modes
	// must also degrade gracefully at runtime.
	Supports(providerType string) bool
	// Wrap builds the dispatcher realizing the strategy over the MCP
	// manager. groups and prefixOf follow Defer's contract.
	Wrap(inner Dispatcher, groups []DeferredGroup, prefixOf func(server string) string) Dispatcher
}

// deferModes is the central mode registry. "normal" — the client-side
// injection strategy, dialect-agnostic — is the floor every other mode
// falls back to.
var deferModes = map[string]DeferMode{
	"normal":       normalDeferMode{},
	"reference":    referenceDeferMode{},
	"tool-search":  toolSearchDeferMode{},
	"system-tools": systemToolsDeferMode{},
}

// DefaultDeferMode is applied when the config names no mode.
const DefaultDeferMode = "normal"

// ResolveDeferMode maps a configured mode name to its implementation,
// falling back to normal — loudly — for unknown names and unsupported
// provider types (the repo-wide capability-mismatch convention: warn and
// degrade, never abort).
func ResolveDeferMode(name, providerType string, warnf func(string, ...any)) DeferMode {
	if name == "" {
		name = DefaultDeferMode
	}
	m, ok := deferModes[name]
	if !ok {
		if warnf != nil {
			warnf("unknown defer_mode %q (using %s)", name, DefaultDeferMode)
		}
		return deferModes[DefaultDeferMode]
	}
	if !m.Supports(providerType) {
		if warnf != nil {
			warnf("defer_mode %q does not apply to provider type %s (using %s)", name, providerType, DefaultDeferMode)
		}
		return deferModes[DefaultDeferMode]
	}
	return m
}

// normalDeferMode is the universal client-side strategy: hide deferred
// schemas behind search_tools and grow the tools array as they load. Costs
// one prompt-cache bust per search; works on every dialect, every model,
// every relay — which is why it is the floor.
type normalDeferMode struct{}

func (normalDeferMode) Name() string         { return "normal" }
func (normalDeferMode) Supports(string) bool { return true }
func (normalDeferMode) Wrap(inner Dispatcher, groups []DeferredGroup, prefixOf func(string) string) Dispatcher {
	return Defer(inner, groups, prefixOf)
}
