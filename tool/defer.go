package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/joyqi/iota/provider"
)

// Deferred tool loading: instead of advertising every MCP tool's schema on
// every request — schemas are the expensive payload, 200–800 tokens each,
// paid per request — a deferred server contributes only a line to the
// search_tools manifest until the model loads its tools on demand. The
// economics: the index (group lines, name lists) is cheap and paid once; the
// payload (schemas) is dear and paid per request — every placement decision
// here moves content from the second bucket into the first.
//
// The model discovers what exists three ways, cheapest first: the group
// lines inside search_tools' own description (bounded by a hard character
// budget — provider description limits are tight and underdocumented, so we
// impose our own), the full catalog behind an empty query, and calling a
// hidden tool DIRECTLY by name — treated as an implicit search-and-call, so
// a model that skips the search step degrades to spending a few extra
// tokens, never to a missing capability.

// DeferredGroup names one deferred server and carries the user-written
// summary from the config `defer` key — the retrieval corpus for the group.
type DeferredGroup struct {
	Name    string
	Summary string
}

// SearchToolName is the meta tool the deferring wrapper advertises.
const SearchToolName = "search_tools"

const (
	// descBudget caps search_tools' description. The tightest known dialect
	// limits sit around 1KB; staying under it keeps the definition legal
	// everywhere without depending on undocumented ceilings.
	descBudget = 800
	// summaryClamp bounds one group line's summary inside the description;
	// the full text always appears in the empty-query catalog.
	summaryClamp = 120
	// catalogDescClamp bounds a tool's one-liner in the catalog — the
	// catalog is an index, not documentation; the full description arrives
	// with the schema when the tool loads.
	catalogDescClamp = 100
	// catalogNamesOnlyAt drops per-tool descriptions from the catalog once
	// the hidden set grows past it: names alone still support exact search
	// and implicit calls, and the catalog lands in history for the session.
	catalogNamesOnlyAt = 50
	// searchTopK caps how many matched tools one search enables — the
	// industry consensus figure (Anthropic's server-side search is fixed at
	// 5; interactive re-searchable designs cluster at 5–10). A generic query
	// ("github") would otherwise enable a whole group in one shot. Exact-name
	// calls (the implicit-load path) are deliberately uncapped.
	searchTopK = 5
)

type deferDispatcher struct {
	inner  Dispatcher
	groups []DeferredGroup
	// prefixOf resolves a group's wire-name prefix ("mcp__<segment>__");
	// "" until the server connects — segments are assigned at connect time.
	prefixOf func(server string) string
	// frozen (the "system-tools" mode): the tools array NEVER grows — loaded
	// schemas queue in pending instead, and the chat loop appends them to
	// history as system messages carrying Tools (the K3 wire shape, which
	// keeps the provider's prompt-cache prefix immutable).
	frozen bool

	mu      sync.Mutex
	enabled map[string]bool    // wire names searched (or implicitly called) in
	pending []provider.ToolDef // frozen mode: loads awaiting TakePendingLoads
}

// Defer wraps a dispatcher (the MCP manager), hiding the deferred groups'
// tools until searched in. inner's capabilities (ApprovalReporter,
// PresentationReporter) pass through untouched.
func Defer(inner Dispatcher, groups []DeferredGroup, prefixOf func(server string) string) Dispatcher {
	return &deferDispatcher{inner: inner, groups: groups, prefixOf: prefixOf, enabled: make(map[string]bool)}
}

// groupView is one group resolved against the live tool set.
type groupView struct {
	DeferredGroup
	prefix string // "" = still connecting
	tools  []provider.ToolDef
}

// resolve snapshots inner.Tools() and buckets the deferred groups' tools.
// The remainder (non-deferred tools) is returned alongside.
func (d *deferDispatcher) resolve() (views []groupView, rest []provider.ToolDef) {
	all := d.inner.Tools()
	views = make([]groupView, len(d.groups))
	for i, g := range d.groups {
		views[i] = groupView{DeferredGroup: g, prefix: d.prefixOf(g.Name)}
	}
	for _, def := range all {
		if def.Name == SearchToolName {
			continue // the wrapper owns this name; a server-side homonym loses
		}
		claimed := false
		for i := range views {
			if views[i].prefix != "" && strings.HasPrefix(def.Name, views[i].prefix) {
				views[i].tools = append(views[i].tools, def)
				claimed = true
				break
			}
		}
		if !claimed {
			rest = append(rest, def)
		}
	}
	return views, rest
}

func (d *deferDispatcher) Tools() []provider.ToolDef {
	views, rest := d.resolve()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []provider.ToolDef{d.searchDefLocked(views)}
	out = append(out, rest...)
	if d.frozen {
		return out // loaded schemas travel via history, never the array
	}
	for _, v := range views {
		for _, def := range v.tools {
			if d.enabled[def.Name] {
				out = append(out, def)
			}
		}
	}
	return out
}

// enableLocked records a load; in frozen mode the schema also queues for the
// history mount (deduplicated — a re-search must not re-append).
func (d *deferDispatcher) enableLocked(def provider.ToolDef) {
	if d.enabled[def.Name] {
		return
	}
	d.enabled[def.Name] = true
	if d.frozen {
		d.pending = append(d.pending, def)
	}
}

// TakePendingLoads drains schemas loaded since the last take (frozen mode).
// The chat loop appends them to history as a system message carrying Tools —
// the append-only mount the K3 protocol requires.
func (d *deferDispatcher) TakePendingLoads() []provider.ToolDef {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.pending
	d.pending = nil
	return out
}

// DeferredTools reports every deferred tool with its load state (/tools).
func (d *deferDispatcher) DeferredTools() []DeferredToolStatus {
	views, _ := d.resolve()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []DeferredToolStatus
	for _, v := range views {
		for _, def := range v.tools {
			state := "deferred"
			if d.enabled[def.Name] {
				state = "loaded"
			}
			out = append(out, DeferredToolStatus{Name: def.Name, Description: def.Description, Group: v.Name, State: state})
		}
	}
	return out
}

// searchDefLocked builds the search_tools definition: a fixed template plus
// as many group lines as the budget admits (level 0 → all lines; level 1 →
// prefix + "+N more"; level 2 → counts only, when even one line won't fit).
func (d *deferDispatcher) searchDefLocked(views []groupView) provider.ToolDef {
	head := "Search and load additional tools before first use. Hidden tool groups:\n"
	foot := "Query by capability keywords (e.g. \"create pull request\"); matched tools load and stay available. " +
		"An empty query lists every hidden tool. Calling a hidden tool directly by name also loads it."

	var lines []string
	for _, v := range views {
		lines = append(lines, "- "+v.Name+" ("+v.countLabel()+"): "+clampLine(v.Summary, summaryClamp))
	}
	body, omitted := fitLines(lines, descBudget-len(head)-len(foot)-1)
	desc := head
	if len(body) == 0 && omitted > 0 {
		groups, tools := len(views), 0
		for _, v := range views {
			tools += len(v.tools)
		}
		desc = fmt.Sprintf("Search and load additional tools before first use. %d tool groups (%d tools) are hidden.\n", groups, tools)
	} else {
		desc += strings.Join(body, "\n") + "\n"
		if omitted > 0 {
			desc += fmt.Sprintf("… +%d more groups — empty query lists all\n", omitted)
		}
	}
	return provider.ToolDef{
		Name:        SearchToolName,
		Description: desc + foot,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Capability keywords; empty lists everything hidden.",
				},
			},
		},
	}
}

func (v groupView) countLabel() string {
	if v.prefix == "" {
		return "connecting…"
	}
	return fmt.Sprintf("%d tools", len(v.tools))
}

// fitLines keeps whole lines within budget, reporting how many were dropped.
func fitLines(lines []string, budget int) (kept []string, omitted int) {
	used := 0
	for i, ln := range lines {
		if used+len(ln)+1 > budget {
			return kept, len(lines) - i
		}
		kept = append(kept, ln)
		used += len(ln) + 1
	}
	return kept, 0
}

// clampLine collapses s to its first line, truncated to max runes.
func clampLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func (d *deferDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name == SearchToolName {
		return d.search(args)
	}
	// Implicit load: a direct call to a hidden-but-known tool enables and
	// runs it in one step — the safety net for models that skip the search.
	views, _ := d.resolve()
	for _, v := range views {
		if v.prefix != "" && strings.HasPrefix(name, v.prefix) {
			d.mu.Lock()
			for _, def := range v.tools {
				if def.Name == name {
					d.enableLocked(def)
					break
				}
			}
			d.mu.Unlock()
			break
		}
	}
	return d.inner.CallTool(ctx, name, args)
}

// search implements the meta tool: keyword scoring over the deferred tools
// — name hits weigh double; descriptions AND parameter names/descriptions
// count too (the corpus the official implementations use: a query like
// "giftwrap" finds the tool whose option mentions it). The top searchTopK
// hits load; an empty query returns the full catalog instead.
func (d *deferDispatcher) search(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	views, _ := d.resolve()
	if strings.TrimSpace(query) == "" {
		return d.catalog(views), false, nil
	}

	var all []provider.ToolDef
	for _, v := range views {
		all = append(all, v.tools...)
	}
	hits := rankTools(all, query)
	if len(hits) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "No tools matched %q. Hidden groups:\n", query)
		for _, v := range views {
			fmt.Fprintf(&b, "- %s (%s): %s\n", v.Name, v.countLabel(), v.Summary)
		}
		b.WriteString("Try different keywords, or an empty query to list every tool.")
		return b.String(), false, nil
	}

	loaded, extra := hits, 0
	if len(loaded) > searchTopK {
		loaded = loaded[:searchTopK]
		extra = len(hits) - searchTopK
	}
	d.mu.Lock()
	for _, def := range loaded {
		d.enableLocked(def)
	}
	d.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "Loaded %d tool(s) — available from the next step:\n", len(loaded))
	for _, def := range loaded {
		fmt.Fprintf(&b, "- %s — %s\n", def.Name, clampLine(def.Description, catalogDescClamp))
	}
	if extra > 0 {
		fmt.Fprintf(&b, "+%d more matched but were not loaded — refine the query, list everything with an empty query, or call a tool by its exact name.\n", extra)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// paramCorpus flattens an input schema's property names and descriptions
// into a lowercase match corpus, recursing through nested objects and array
// items — aligned with the official implementations, which search "tool
// names, descriptions, argument names, and argument descriptions".
func paramCorpus(schema map[string]any) string {
	if schema == nil {
		return ""
	}
	var b strings.Builder
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		if props, ok := m["properties"].(map[string]any); ok {
			for name, v := range props {
				b.WriteString(name)
				b.WriteByte(' ')
				if pm, ok := v.(map[string]any); ok {
					if desc, ok := pm["description"].(string); ok {
						b.WriteString(desc)
						b.WriteByte(' ')
					}
					walk(pm)
				}
			}
		}
		if items, ok := m["items"].(map[string]any); ok {
			walk(items)
		}
	}
	walk(schema)
	return strings.ToLower(b.String())
}

// rankTools scores defs against query terms — name hits weigh double;
// descriptions and the flattened parameter corpus count singly — returning
// matches sorted by score (ties by name, so selection is deterministic).
// Shared by the client-side search and the protocol modes' search seam.
func rankTools(defs []provider.ToolDef, query string) []provider.ToolDef {
	terms := strings.Fields(strings.ToLower(query))
	type hit struct {
		def   provider.ToolDef
		score int
	}
	var hits []hit
	for _, def := range defs {
		name, desc := strings.ToLower(def.Name), strings.ToLower(def.Description)
		params := paramCorpus(def.InputSchema)
		score := 0
		for _, t := range terms {
			if strings.Contains(name, t) {
				score += 2
			}
			if strings.Contains(desc, t) {
				score++
			}
			if strings.Contains(params, t) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{def, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].def.Name < hits[j].def.Name
	})
	out := make([]provider.ToolDef, len(hits))
	for i, h := range hits {
		out[i] = h.def
	}
	return out
}

// catalog is the empty-query listing: ALWAYS every group line with its full
// summary (whatever the description had to omit), then the per-tool index —
// one-liners up to catalogNamesOnlyAt tools, names only beyond.
func (d *deferDispatcher) catalog(views []groupView) string {
	total := 0
	for _, v := range views {
		total += len(v.tools)
	}
	namesOnly := total > catalogNamesOnlyAt

	d.mu.Lock()
	defer d.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "Hidden tool groups (%d groups, %d tools):\n", len(views), total)
	for _, v := range views {
		fmt.Fprintf(&b, "\n%s (%s): %s\n", v.Name, v.countLabel(), v.Summary)
		for _, def := range v.tools {
			mark := ""
			if d.enabled[def.Name] {
				mark = " (loaded)"
			}
			if namesOnly {
				fmt.Fprintf(&b, "  %s%s\n", def.Name, mark)
			} else {
				fmt.Fprintf(&b, "  %s — %s%s\n", def.Name, clampLine(def.Description, catalogDescClamp), mark)
			}
		}
	}
	b.WriteString("\nQuery by capability keywords to load tools, or call a listed tool directly to load and run it in one step.")
	return b.String()
}

// Owns reports name ownership including HIDDEN tools — Merge routes direct
// calls here so the implicit-load path works through the merged dispatcher.
func (d *deferDispatcher) Owns(name string) bool {
	if name == SearchToolName {
		return true
	}
	for _, def := range d.inner.Tools() {
		if def.Name == name {
			return true
		}
	}
	return false
}

// RequiresApproval / Presentation delegate to the wrapped dispatcher.
func (d *deferDispatcher) RequiresApproval(name string) bool {
	if ar, ok := d.inner.(ApprovalReporter); ok {
		return ar.RequiresApproval(name)
	}
	return false
}

func (d *deferDispatcher) Presentation(name string) Presentation {
	if pr, ok := d.inner.(PresentationReporter); ok {
		return pr.Presentation(name)
	}
	return PresentGroup
}
