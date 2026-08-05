package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reference mode wire (anthropic): deferred tools emit defer_loading and
// summon the server-side regex search tool; non-deferred tools carry
// neither. Without any deferred tool the search tool must not appear.
func TestAnthropicDeferLoadingWire(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	p := NewAnthropic("sk-test", srv.URL, "claude-sonnet-5", nil, srv.Client())
	var sink strings.Builder
	p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "go"}},
		[]ToolDef{
			{Name: "plain", Description: "d", InputSchema: map[string]any{"properties": map[string]any{}}},
			{Name: "mcp__gh__pr", Description: "d", InputSchema: map[string]any{"properties": map[string]any{}}, Deferred: true},
		},
		&sink, nopWC{})

	tools, _ := got["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %d entries, want plain + deferred + search tool:\n%v", len(tools), got["tools"])
	}
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if _, has := byName["plain"]["defer_loading"]; has {
		t.Error("plain tool must not carry defer_loading")
	}
	if byName["mcp__gh__pr"]["defer_loading"] != true {
		t.Error("deferred tool must carry defer_loading: true")
	}
	search := byName["tool_search_tool_regex"]
	if search == nil || search["type"] != "tool_search_tool_regex_20251119" {
		t.Errorf("server search tool missing/wrong: %v", search)
	}
	if _, has := search["input_schema"]; has {
		t.Error("the search tool must not emit input_schema")
	}
}

type nopWC struct{}

func (nopWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopWC) Close() error                { return nil }

// The tool-search mode wire (openresponses): with a searcher installed,
// deferred tools emit defer_loading behind a tool_search entry; a
// tool_search_call ends leg 1, the client answers with tool_search_output
// (call replayed verbatim + mounted specs), and leg 2 completes the text.
func TestOpenResponsesToolSearchLoop(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			w.Write([]byte(`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[]}}` + "\n\n" +
				`data: {"type":"response.output_item.done","item":{"id":"ts_1","type":"tool_search_call","call_id":"tsc_1","status":"completed","execution":"client","arguments":{"query":"pull request"}}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
			return
		}
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"done"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewOpenResponses("sk-test", srv.URL, "gpt-5.5", nil, srv.Client())
	p.SetToolSearcher(func(query string) []ToolDef {
		if query != "pull request" {
			t.Errorf("searcher got query %q", query)
		}
		return []ToolDef{{Name: "mcp__gh__pr", Description: "PRs", InputSchema: map[string]any{"type": "object"}}}
	})

	var sink strings.Builder
	full, _, calls, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "go"}},
		[]ToolDef{{Name: "mcp__gh__pr", Description: "PRs", InputSchema: map[string]any{"type": "object"}, Deferred: true}},
		&sink, nopWC{})
	if err != nil || full != "done" || len(calls) != 0 {
		t.Fatalf("full=%q calls=%d err=%v", full, len(calls), err)
	}
	if len(bodies) != 2 {
		t.Fatalf("legs = %d, want 2", len(bodies))
	}

	// Leg 1: deferred function + tool_search entry.
	tools1, _ := bodies[0]["tools"].([]any)
	var sawSearch, sawDeferred bool
	for _, tl := range tools1 {
		m := tl.(map[string]any)
		if m["type"] == "tool_search" && m["execution"] == "client" {
			sawSearch = true
		}
		if m["name"] == "mcp__gh__pr" && m["defer_loading"] == true {
			sawDeferred = true
		}
	}
	if !sawSearch || !sawDeferred {
		t.Fatalf("leg-1 tools wrong: %v", bodies[0]["tools"])
	}

	// Leg 2 input: the replayed call + our tool_search_output with the spec.
	in2, _ := json.Marshal(bodies[1]["input"])
	// The reasoning item MUST replay with its paired tool_search_call —
	// gpt-5.x rejects the call without it (live-API finding).
	for _, want := range []string{`"rs_1"`, `"tool_search_call"`, `"tsc_1"`, `"tool_search_output"`, `"mcp__gh__pr"`, `"completed"`} {
		if !strings.Contains(string(in2), want) {
			t.Errorf("leg-2 input missing %s:\n%s", want, in2)
		}
	}
}

// The system-tools mode wire (chatcomp): a system message carrying Tools
// serializes as role system + tools and NO content key (the K3 constraint).
func TestOpenAISystemToolsMessageWire(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAI("sk-test", srv.URL, "kimi-k3", nil, srv.Client())
	var sink strings.Builder
	p.StreamChatWithTools(context.Background(), []Message{
		{Role: "user", Content: "go"},
		{Role: "system", Tools: []ToolDef{{Name: "loaded_tool", Description: "d", InputSchema: map[string]any{"type": "object"}}}},
	}, nil, &sink, nopWC{})

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	tm := msgs[1].(map[string]any)
	if tm["role"] != "system" {
		t.Fatalf("mount message role = %v", tm["role"])
	}
	if _, has := tm["content"]; has {
		t.Fatal("the tools mount must carry NO content key (K3 400s otherwise)")
	}
	tl, _ := tm["tools"].([]any)
	if len(tl) != 1 || tl[0].(map[string]any)["function"].(map[string]any)["name"] != "loaded_tool" {
		t.Fatalf("mounted tools wrong: %v", tm["tools"])
	}
}
