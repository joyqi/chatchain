package tool

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
)

// fakeMCP is a stand-in for the MCP manager: a live tool list plus recorded
// calls, with approval/presentation capabilities to verify pass-through.
type fakeMCP struct {
	tools  []provider.ToolDef
	called []string
}

func (f *fakeMCP) Tools() []provider.ToolDef { return f.tools }
func (f *fakeMCP) CallTool(_ context.Context, name string, _ map[string]any) (string, bool, error) {
	f.called = append(f.called, name)
	return "ok:" + name, false, nil
}
func (f *fakeMCP) RequiresApproval(name string) bool     { return name == "mcp__gh__danger" }
func (f *fakeMCP) Presentation(name string) Presentation { return PresentGroup }
func (f *fakeMCP) staticPrefix(server string) func(string) string {
	return func(s string) string {
		if s == server {
			return "mcp__gh__"
		}
		return ""
	}
}

func newDeferFixture() (*fakeMCP, Dispatcher) {
	inner := &fakeMCP{tools: []provider.ToolDef{
		{Name: "mcp__gh__create_pr", Description: "Create a pull request on GitHub"},
		{Name: "mcp__gh__search_code", Description: "Search code across repositories"},
		{Name: "mcp__gh__danger", Description: "Force-push a branch"},
		{Name: "mcp__fs__read", Description: "Read a file"},
	}}
	d := Defer(inner, []DeferredGroup{{Name: "github", Summary: "GitHub repos, issues, PRs"}},
		inner.staticPrefix("github"))
	return inner, d
}

// Deferred tools hide until searched in; non-deferred servers and the
// search_tools entry pass through.
func TestDeferHidesUntilSearched(t *testing.T) {
	_, d := newDeferFixture()

	names := toolNames(d.Tools())
	if !names[SearchToolName] || !names["mcp__fs__read"] {
		t.Fatalf("search_tools and non-deferred tools must be advertised: %v", names)
	}
	if names["mcp__gh__create_pr"] || names["mcp__gh__danger"] {
		t.Fatalf("deferred tools leaked before any search: %v", names)
	}

	out, isErr, err := d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "pull request"})
	if err != nil || isErr {
		t.Fatalf("search failed: %q %v", out, err)
	}
	if !strings.Contains(out, "mcp__gh__create_pr") {
		t.Fatalf("search result must name the loaded tool:\n%s", out)
	}
	names = toolNames(d.Tools())
	if !names["mcp__gh__create_pr"] {
		t.Fatal("searched tool must be advertised afterwards")
	}
	if names["mcp__gh__danger"] {
		t.Fatal("unmatched tools must stay hidden")
	}
}

// A direct call to a hidden tool is an implicit search-and-call: it executes
// AND enables the tool for subsequent rounds.
func TestDeferImplicitLoadOnDirectCall(t *testing.T) {
	inner, d := newDeferFixture()

	out, isErr, err := d.CallTool(context.Background(), "mcp__gh__danger", nil)
	if err != nil || isErr || out != "ok:mcp__gh__danger" {
		t.Fatalf("implicit call failed: %q %v %v", out, isErr, err)
	}
	if len(inner.called) != 1 || inner.called[0] != "mcp__gh__danger" {
		t.Fatalf("inner not called: %v", inner.called)
	}
	if !toolNames(d.Tools())["mcp__gh__danger"] {
		t.Fatal("implicitly called tool must be advertised afterwards")
	}
}

// The implicit-load path must survive Merge: the merged dispatcher routes a
// hidden name through the Owner capability, and approval/presentation
// queries reach the wrapped dispatcher the same way.
func TestDeferOwnsThroughMerge(t *testing.T) {
	_, d := newDeferFixture()
	merged := Merge(d)

	if out, _, err := merged.CallTool(context.Background(), "mcp__gh__create_pr", nil); err != nil || out != "ok:mcp__gh__create_pr" {
		t.Fatalf("merged call to hidden tool: %q %v", out, err)
	}
	ar := merged.(ApprovalReporter)
	if !ar.RequiresApproval("mcp__gh__danger") {
		t.Fatal("approval must route through Owner to the wrapped dispatcher")
	}
	if ar.RequiresApproval("mcp__gh__create_pr") {
		t.Fatal("non-approval tool misreported")
	}
}

// The empty query returns the catalog: every group line with its FULL
// summary, per-tool one-liners, and (loaded) markers.
func TestDeferEmptyQueryCatalog(t *testing.T) {
	_, d := newDeferFixture()
	d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "pull request"})

	out, _, _ := d.CallTool(context.Background(), SearchToolName, map[string]any{"query": ""})
	for _, want := range []string{
		"github (3 tools): GitHub repos, issues, PRs",
		"mcp__gh__create_pr — Create a pull request on GitHub (loaded)",
		"mcp__gh__danger — Force-push a branch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mcp__fs__read") {
		t.Errorf("non-deferred tools must not clutter the catalog:\n%s", out)
	}
}

// Past catalogNamesOnlyAt tools the catalog degrades to names only — it
// lands in history for the whole session and must stay bounded.
func TestDeferCatalogNamesOnlyWhenLarge(t *testing.T) {
	inner := &fakeMCP{}
	for i := 0; i < catalogNamesOnlyAt+10; i++ {
		inner.tools = append(inner.tools, provider.ToolDef{
			Name:        fmt.Sprintf("mcp__gh__tool_%02d", i),
			Description: "A described tool",
		})
	}
	d := Defer(inner, []DeferredGroup{{Name: "github", Summary: "big"}}, inner.staticPrefix("github"))

	out, _, _ := d.CallTool(context.Background(), SearchToolName, map[string]any{})
	if strings.Contains(out, "— A described tool") {
		t.Fatalf("large catalog must drop per-tool descriptions:\n%s", out)
	}
	if !strings.Contains(out, "mcp__gh__tool_00") {
		t.Fatalf("names must survive the degradation:\n%s", out)
	}
}

// The search_tools description holds group lines within the budget: long
// summaries clamp per line, and overflowing groups fold into a "+N more"
// tail rather than blowing the provider's description limit.
func TestDeferDescriptionBudget(t *testing.T) {
	inner := &fakeMCP{tools: []provider.ToolDef{{Name: "mcp__gh__a", Description: "x"}}}
	long := strings.Repeat("very long summary ", 20)
	var groups []DeferredGroup
	for i := 0; i < 12; i++ {
		groups = append(groups, DeferredGroup{Name: fmt.Sprintf("srv%02d", i), Summary: long})
	}
	d := Defer(inner, groups, func(string) string { return "" })

	def := d.Tools()[0]
	if def.Name != SearchToolName {
		t.Fatalf("first advertised tool = %q", def.Name)
	}
	if len(def.Description) > descBudget+100 {
		t.Fatalf("description %d chars, budget %d", len(def.Description), descBudget)
	}
	if !strings.Contains(def.Description, "more groups") {
		t.Fatalf("overflow must fold into a +N more tail:\n%s", def.Description)
	}
}

// A group whose server has not connected yet shows as connecting; its tools
// cannot match a search, and the group line still names the capability.
func TestDeferConnectingGroup(t *testing.T) {
	inner := &fakeMCP{}
	d := Defer(inner, []DeferredGroup{{Name: "slack", Summary: "Send Slack messages"}},
		func(string) string { return "" })

	def := d.Tools()[0]
	if !strings.Contains(def.Description, "slack (connecting…): Send Slack messages") {
		t.Fatalf("connecting group line missing:\n%s", def.Description)
	}
	out, _, _ := d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "slack"})
	if !strings.Contains(out, "No tools matched") || !strings.Contains(out, "connecting…") {
		t.Fatalf("search against a connecting group must say so:\n%s", out)
	}
}

// A server-side tool that happens to be named search_tools loses to the
// wrapper — one owner for the name, no duplicate defs.
func TestDeferSearchToolsNameCollision(t *testing.T) {
	inner := &fakeMCP{tools: []provider.ToolDef{
		{Name: SearchToolName, Description: "impostor"},
		{Name: "mcp__fs__read", Description: "Read a file"},
	}}
	d := Defer(inner, []DeferredGroup{{Name: "x", Summary: "y"}}, func(string) string { return "" })

	count := 0
	for _, def := range d.Tools() {
		if def.Name == SearchToolName {
			count++
			if strings.Contains(def.Description, "impostor") {
				t.Fatal("the impostor's definition leaked")
			}
		}
	}
	if count != 1 {
		t.Fatalf("search_tools advertised %d times", count)
	}
}

func toolNames(defs []provider.ToolDef) map[string]bool {
	m := make(map[string]bool, len(defs))
	for _, d := range defs {
		m[d.Name] = true
	}
	return m
}

// A search enables at most searchTopK tools — a generic query must not dump
// a whole group into the tools array. Ties break by name, so the loaded set
// is deterministic; the result names the overflow and the escape hatches.
func TestDeferSearchTopK(t *testing.T) {
	inner := &fakeMCP{}
	for i := 0; i < searchTopK+3; i++ {
		inner.tools = append(inner.tools, provider.ToolDef{
			Name:        fmt.Sprintf("mcp__gh__widget_%02d", i),
			Description: "Operate a github widget",
		})
	}
	d := Defer(inner, []DeferredGroup{{Name: "github", Summary: "widgets"}}, inner.staticPrefix("github"))

	out, _, _ := d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "github widget"})
	if !strings.Contains(out, fmt.Sprintf("Loaded %d tool(s)", searchTopK)) {
		t.Fatalf("must load exactly top-K:\n%s", out)
	}
	if !strings.Contains(out, "+3 more matched") {
		t.Fatalf("overflow must be named:\n%s", out)
	}
	names := toolNames(d.Tools())
	for i := 0; i < searchTopK; i++ {
		if !names[fmt.Sprintf("mcp__gh__widget_%02d", i)] {
			t.Errorf("widget_%02d should be loaded (name tie-break)", i)
		}
	}
	for i := searchTopK; i < searchTopK+3; i++ {
		if names[fmt.Sprintf("mcp__gh__widget_%02d", i)] {
			t.Errorf("widget_%02d must stay hidden past top-K", i)
		}
	}
}

// The match corpus includes parameter names and descriptions: a term that
// appears only inside the input schema still finds the tool.
func TestDeferParamCorpusMatch(t *testing.T) {
	inner := &fakeMCP{tools: []provider.ToolDef{
		{Name: "mcp__gh__submit_order", Description: "Submit a customer order",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"items": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"options": map[string]any{"type": "object", "properties": map[string]any{
								"giftwrap": map[string]any{"type": "boolean", "description": "Wrap the item as a gift"},
							}},
						},
					}},
				},
			}},
		{Name: "mcp__gh__other", Description: "Unrelated"},
	}}
	d := Defer(inner, []DeferredGroup{{Name: "github", Summary: "shop"}}, inner.staticPrefix("github"))

	out, _, _ := d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "giftwrap"})
	if !strings.Contains(out, "mcp__gh__submit_order") {
		t.Fatalf("param-name match must find the tool:\n%s", out)
	}
	if strings.Contains(out, "mcp__gh__other") {
		t.Fatalf("unrelated tool must not match:\n%s", out)
	}
}

// ResolveDeferMode: "" and "normal" resolve to the normal strategy; unknown
// names and (future) unsupported provider types warn and fall back — the
// floor is always normal, never an abort.
func TestResolveDeferMode(t *testing.T) {
	var warns []string
	warnf := func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }

	if m := ResolveDeferMode("", "openai", warnf); m.Name() != "normal" {
		t.Fatalf("empty mode = %q, want normal", m.Name())
	}
	if m := ResolveDeferMode("normal", "anthropic", warnf); m.Name() != "normal" {
		t.Fatalf("normal mode = %q", m.Name())
	}
	if len(warns) != 0 {
		t.Fatalf("valid modes must not warn: %v", warns)
	}

	if m := ResolveDeferMode("quantum", "openai", warnf); m.Name() != "normal" {
		t.Fatalf("unknown mode must fall back to normal, got %q", m.Name())
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "quantum") {
		t.Fatalf("unknown mode must warn loudly: %v", warns)
	}

	// The mode's wrapper is the real deferring dispatcher.
	inner := &fakeMCP{tools: []provider.ToolDef{{Name: "mcp__gh__a", Description: "x"}}}
	d := ResolveDeferMode("normal", "openai", nil).Wrap(inner,
		[]DeferredGroup{{Name: "github", Summary: "s"}}, inner.staticPrefix("github"))
	names := toolNames(d.Tools())
	if !names[SearchToolName] || names["mcp__gh__a"] {
		t.Fatalf("wrapped dispatcher must defer: %v", names)
	}
}

// The protocol modes' wrappers: reference/tool-search MARK deferred tools
// (no hiding, no search_tools of ours); the searching variant answers the
// provider's client-executed search capped at top-K; system-tools freezes
// the array and queues loads for the history mount.
func TestProtocolModeWrappers(t *testing.T) {
	inner := &fakeMCP{tools: []provider.ToolDef{
		{Name: "mcp__gh__pr", Description: "Create a pull request"},
		{Name: "mcp__fs__read", Description: "Read a file"},
	}}
	groups := []DeferredGroup{{Name: "github", Summary: "gh"}}

	marked := ResolveDeferMode("reference", "anthropic", nil).Wrap(inner, groups, inner.staticPrefix("github"))
	names := toolNames(marked.Tools())
	if names[SearchToolName] {
		t.Fatal("protocol modes must not advertise our search_tools")
	}
	var deferredMark, plainMark bool
	for _, def := range marked.Tools() {
		if def.Name == "mcp__gh__pr" {
			deferredMark = def.Deferred
		}
		if def.Name == "mcp__fs__read" {
			plainMark = def.Deferred
		}
	}
	if !deferredMark || plainMark {
		t.Fatalf("marking wrong: deferred=%v plain=%v", deferredMark, plainMark)
	}

	searching := ResolveDeferMode("tool-search", "openresponses", nil).Wrap(inner, groups, inner.staticPrefix("github"))
	ts, ok := searching.(ToolSearcher)
	if !ok {
		t.Fatal("tool-search wrapper must implement ToolSearcher")
	}
	hits := ts.SearchTools("pull request")
	if len(hits) != 1 || hits[0].Name != "mcp__gh__pr" {
		t.Fatalf("SearchTools = %v", hits)
	}

	frozen := ResolveDeferMode("system-tools", "openai", nil).Wrap(inner, groups, inner.staticPrefix("github"))
	frozen.CallTool(context.Background(), SearchToolName, map[string]any{"query": "pull request"})
	if toolNames(frozen.Tools())["mcp__gh__pr"] {
		t.Fatal("frozen mode must never grow the tools array")
	}
	pl := frozen.(PendingLoader)
	defs := pl.TakePendingLoads()
	if len(defs) != 1 || defs[0].Name != "mcp__gh__pr" {
		t.Fatalf("pending loads = %v", defs)
	}
	if len(pl.TakePendingLoads()) != 0 {
		t.Fatal("loads must drain exactly once")
	}
	// A re-search of the same tool must not re-queue it.
	frozen.CallTool(context.Background(), SearchToolName, map[string]any{"query": "pull request"})
	if len(pl.TakePendingLoads()) != 0 {
		t.Fatal("re-search re-queued an already-loaded tool")
	}
}

// Mode capability gates: wrong provider types fall back loudly to normal.
func TestProtocolModeSupports(t *testing.T) {
	var warns []string
	warnf := func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }
	for _, c := range []struct{ mode, ptype, want string }{
		{"reference", "anthropic", "reference"},
		{"reference", "openai", "normal"},
		{"tool-search", "openresponses", "tool-search"},
		{"tool-search", "anthropic", "normal"},
		{"system-tools", "openai", "system-tools"},
		{"system-tools", "openresponses", "normal"},
	} {
		if got := ResolveDeferMode(c.mode, c.ptype, warnf).Name(); got != c.want {
			t.Errorf("%s on %s = %s, want %s", c.mode, c.ptype, got, c.want)
		}
	}
	if len(warns) != 3 {
		t.Errorf("unsupported combos must warn, got %d warnings", len(warns))
	}
}

// DeferInspector: the normal wrapper reports per-tool load state; the
// protocol wrappers report everything delegated; Merge aggregates.
func TestDeferInspector(t *testing.T) {
	inner, d := newDeferFixture()
	_ = inner
	d.CallTool(context.Background(), SearchToolName, map[string]any{"query": "pull request"})

	merged := Merge(d)
	sts := merged.(DeferInspector).DeferredTools()
	byName := map[string]DeferredToolStatus{}
	for _, st := range sts {
		byName[st.Name] = st
	}
	if st := byName["mcp__gh__create_pr"]; st.State != "loaded" || st.Group != "github" {
		t.Fatalf("loaded state wrong: %+v", st)
	}
	if st := byName["mcp__gh__danger"]; st.State != "deferred" {
		t.Fatalf("hidden state wrong: %+v", st)
	}

	marked := ResolveDeferMode("reference", "anthropic", nil).Wrap(inner,
		[]DeferredGroup{{Name: "github", Summary: "gh"}}, inner.staticPrefix("github"))
	for _, st := range marked.(DeferInspector).DeferredTools() {
		if st.State != "deferred (protocol)" {
			t.Fatalf("protocol state wrong: %+v", st)
		}
	}
}
