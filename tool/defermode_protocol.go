package tool

import (
	"context"
	"strings"

	"chatchain/provider"
)

// Protocol-backed defer modes. Unlike normal (which hides schemas behind the
// client-side search_tools), these delegate the deferral to the provider's
// own wire protocol — the dispatcher's job shrinks to MARKING which tools
// are deferred (ToolDef.Deferred) and, for client-executed search, answering
// the provider's search callback. Schemas stay in the declaration channel;
// only their placement moves out of the cached prefix.

// ToolSearcher is the client-executed search seam: the provider (openresponses
// tool_search with execution "client") forwards the model's search queries
// here and mounts whatever comes back. Merge forwards it to the first part
// that implements it.
type ToolSearcher interface {
	SearchTools(query string) []provider.ToolDef
}

// DeferredToolStatus describes one deferred tool for the /tools view.
type DeferredToolStatus struct {
	Name        string
	Description string
	Group       string // server config name
	State       string // "deferred" | "loaded" | "deferred (protocol)"
}

// DeferInspector exposes deferred-tool state for diagnostics (/tools). Merge
// aggregates from every implementing part.
type DeferInspector interface {
	DeferredTools() []DeferredToolStatus
}

// DeferredTools (protocol modes): every marked tool is delegated to the
// provider's protocol — the client does not track loads.
func (m *markedDispatcher) DeferredTools() []DeferredToolStatus {
	var out []DeferredToolStatus
	for _, g := range m.groups {
		p := m.prefixOf(g.Name)
		if p == "" {
			continue
		}
		for _, def := range m.inner.Tools() {
			if strings.HasPrefix(def.Name, p) {
				out = append(out, DeferredToolStatus{Name: def.Name, Description: def.Description, Group: g.Name, State: "deferred (protocol)"})
			}
		}
	}
	return out
}

// PendingLoader is the frozen-mount seam (system-tools): the chat loop
// drains schemas loaded since the last round and appends them to history as
// a system message carrying Tools. Merge forwards to every implementing part.
type PendingLoader interface {
	TakePendingLoads() []provider.ToolDef
}

// markedDispatcher passes every tool through, marking the deferred groups'
// tools Deferred — the provider's protocol handles hiding, search, and
// mounting. No search_tools meta tool: the protocol brings its own.
type markedDispatcher struct {
	inner    Dispatcher
	groups   []DeferredGroup
	prefixOf func(server string) string
}

func (m *markedDispatcher) deferredPrefix(name string) bool {
	for _, g := range m.groups {
		if p := m.prefixOf(g.Name); p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (m *markedDispatcher) Tools() []provider.ToolDef {
	all := m.inner.Tools()
	out := make([]provider.ToolDef, len(all))
	for i, def := range all {
		if m.deferredPrefix(def.Name) {
			def.Deferred = true
		}
		out[i] = def
	}
	return out
}

func (m *markedDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	return m.inner.CallTool(ctx, name, args)
}

func (m *markedDispatcher) RequiresApproval(name string) bool {
	if ar, ok := m.inner.(ApprovalReporter); ok {
		return ar.RequiresApproval(name)
	}
	return false
}

func (m *markedDispatcher) Presentation(name string) Presentation {
	if pr, ok := m.inner.(PresentationReporter); ok {
		return pr.Presentation(name)
	}
	return PresentGroup
}

// searchingDispatcher adds the client-executed search capability over the
// marked passthrough: the provider forwards tool_search_call queries here.
type searchingDispatcher struct {
	markedDispatcher
}

// SearchTools ranks the deferred tools against the query (shared scoring:
// name double-weighted, description, parameter corpus) and returns the top
// searchTopK — the provider mounts them via its protocol's output item.
func (s *searchingDispatcher) SearchTools(query string) []provider.ToolDef {
	var deferred []provider.ToolDef
	for _, def := range s.Tools() {
		if def.Deferred {
			deferred = append(deferred, def)
		}
	}
	hits := rankTools(deferred, query)
	if len(hits) > searchTopK {
		hits = hits[:searchTopK]
	}
	return hits
}

// referenceDeferMode: Anthropic defer_loading. The dialect emits deferred
// tools with defer_loading true plus the server-side regex search tool;
// search and mounting happen server-side (tool_reference expansion), so the
// dispatcher only marks.
type referenceDeferMode struct{}

func (referenceDeferMode) Name() string                      { return "reference" }
func (referenceDeferMode) Supports(providerType string) bool { return providerType == "anthropic" }
func (referenceDeferMode) Wrap(inner Dispatcher, groups []DeferredGroup, prefixOf func(string) string) Dispatcher {
	return &markedDispatcher{inner: inner, groups: groups, prefixOf: prefixOf}
}

// toolSearchDeferMode: OpenAI Responses tool_search with client execution.
// The dialect emits deferred tools with defer_loading plus a client-executed
// tool_search entry; the model's searches route back through SearchTools.
type toolSearchDeferMode struct{}

func (toolSearchDeferMode) Name() string                      { return "tool-search" }
func (toolSearchDeferMode) Supports(providerType string) bool { return providerType == "openresponses" }
func (toolSearchDeferMode) Wrap(inner Dispatcher, groups []DeferredGroup, prefixOf func(string) string) Dispatcher {
	return &searchingDispatcher{markedDispatcher{inner: inner, groups: groups, prefixOf: prefixOf}}
}

// systemToolsDeferMode: Kimi K3 dynamically-loaded tools. Reuses the normal
// client-side search flow, but the MOUNT is frozen: the tools array never
// grows — loaded schemas queue behind PendingLoader and the chat loop
// appends them to history as system messages carrying Tools (no content,
// append-only: the K3 constraints). OpenAI-compatible dialect against
// Moonshot directly, kimi-k3 only — other models fail server-side.
type systemToolsDeferMode struct{}

func (systemToolsDeferMode) Name() string                      { return "system-tools" }
func (systemToolsDeferMode) Supports(providerType string) bool { return providerType == "openai" }
func (systemToolsDeferMode) Wrap(inner Dispatcher, groups []DeferredGroup, prefixOf func(string) string) Dispatcher {
	return &deferDispatcher{inner: inner, groups: groups, prefixOf: prefixOf,
		frozen: true, enabled: make(map[string]bool)}
}
