package chat

import (
	"context"
	"strings"
	"testing"

	"chatchain/provider"
	"chatchain/tool"
)

// deferStatusDispatcher advertises defs and reports deferred state — the
// /tools view fixture.
type deferStatusDispatcher struct {
	defs   []provider.ToolDef
	status []tool.DeferredToolStatus
}

func (d deferStatusDispatcher) Tools() []provider.ToolDef { return d.defs }
func (d deferStatusDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	return "ok", false, nil
}
func (d deferStatusDispatcher) DeferredTools() []tool.DeferredToolStatus { return d.status }

// The /tools list shows deferred-but-hidden tools dimmed after the advertised
// set, with their group and state, and the header carries the tally.
func TestToolStatusLinesDeferred(t *testing.T) {
	dispatch := deferStatusDispatcher{
		defs: []provider.ToolDef{
			{Name: "search_tools", Description: "meta"},
			{Name: "mcp__seo__serp", Description: "SERP lookup"},
		},
		status: []tool.DeferredToolStatus{
			{Name: "mcp__seo__serp", Description: "SERP lookup", Group: "DataForSEO", State: "loaded"},
			{Name: "mcp__seo__keywords", Description: "keyword data", Group: "DataForSEO", State: "deferred"},
			{Name: "mcp__cdt__click", Description: "click an element", Group: "chrome-devtools", State: "deferred"},
		},
	}

	lines := toolStatusLines(dispatch, nil)
	var text []string
	for _, l := range lines {
		text = append(text, stripANSI(l))
	}

	if want := "2 tool(s) available · 3 deferred (1 loaded)"; text[0] != want {
		t.Fatalf("header = %q, want %q", text[0], want)
	}
	// Hidden tools sort after the advertised set, grouped by server name
	// (byte order), each row naming group and state.
	if len(text) != 5 {
		t.Fatalf("lines = %d, want header + 2 advertised + 2 hidden:\n%s", len(text), strings.Join(text, "\n"))
	}
	if !strings.Contains(text[3], "seo:keywords") || !strings.Contains(text[3], "[mcp: DataForSEO · deferred]") {
		t.Errorf("hidden row 1 = %q", text[3])
	}
	if !strings.Contains(text[4], "cdt:click") || !strings.Contains(text[4], "[mcp: chrome-devtools · deferred]") {
		t.Errorf("hidden row 2 = %q", text[4])
	}
	// The loaded tool is advertised, not repeated in the hidden tail.
	joined := strings.Join(text[3:], "\n")
	if strings.Contains(joined, "serp") {
		t.Errorf("loaded tool leaked into the hidden tail:\n%s", joined)
	}
}

// Without a DeferInspector the header stays bare — the pre-defer contract.
func TestToolStatusLinesNoDefer(t *testing.T) {
	lines := toolStatusLines(noopDispatcher{}, nil)
	head := stripANSI(lines[0])
	if head != "1 tool(s) available" {
		t.Fatalf("header = %q, want no deferred suffix", head)
	}
}

// All tools hidden (nothing advertised yet) still renders the list — not
// "No tools available".
func TestToolStatusLinesAllHidden(t *testing.T) {
	dispatch := deferStatusDispatcher{
		status: []tool.DeferredToolStatus{
			{Name: "mcp__x__a", Description: "d", Group: "x", State: "deferred (protocol)"},
		},
	}
	lines := toolStatusLines(dispatch, nil)
	text := stripANSI(strings.Join(lines, "\n"))
	if strings.Contains(text, "No tools available") {
		t.Fatalf("hidden-only set must still list:\n%s", text)
	}
	if !strings.Contains(text, "0 tool(s) available · 1 deferred (0 loaded)") || !strings.Contains(text, "deferred (protocol)") {
		t.Fatalf("unexpected render:\n%s", text)
	}
}
