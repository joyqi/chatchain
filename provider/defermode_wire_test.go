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

// Server search blocks (reference mode) are captured from the stream —
// server_tool_use recomposed from its input deltas, tool_search_tool_result
// verbatim from its start event — exposed via LastRawContent, and must
// round-trip through Marshal/Unmarshal. The server search must NOT raise the
// tool-call widget observer.
func TestAnthropicServerBlockCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"tool_search_tool_regex","input":{}}}` + "\n\n" +
				`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\"weather\"}"}}` + "\n\n" +
				`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srv_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"atmos_query"}]}}}` + "\n\n" +
				`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"found it"}}` + "\n\n" +
				`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewAnthropic("sk-test", srv.URL, "claude-sonnet-5", nil, srv.Client())
	var observed []string
	p.SetToolCallObserver(func(name, delta string) { observed = append(observed, name) })
	var sink strings.Builder
	// Capture is gated on the reference protocol being active: the request
	// must carry a deferred tool.
	full, _, _, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "go"}},
		[]ToolDef{{Name: "atmos_query", Description: "d", InputSchema: map[string]any{"type": "object"}, Deferred: true}},
		&sink, nopWC{})
	if err != nil || full != "found it" {
		t.Fatalf("full=%q err=%v", full, err)
	}
	if len(observed) != 0 {
		t.Errorf("server search deltas must not reach the tool observer, got %v", observed)
	}

	raw, ok := p.LastRawContent().(*anthropicRawBlocks)
	if !ok || len(raw.Blocks) != 2 {
		t.Fatalf("LastRawContent = %#v, want 2 server blocks", p.LastRawContent())
	}
	if !strings.Contains(string(raw.Blocks[0]), `"server_tool_use"`) ||
		!strings.Contains(string(raw.Blocks[0]), `"pattern":"weather"`) {
		t.Errorf("block 0 not recomposed with its input: %s", raw.Blocks[0])
	}
	if !strings.Contains(string(raw.Blocks[1]), `"tool_search_tool_result"`) ||
		!strings.Contains(string(raw.Blocks[1]), `"atmos_query"`) {
		t.Errorf("block 1 not captured verbatim: %s", raw.Blocks[1])
	}

	// Persistence round-trip.
	blob, err := p.MarshalRawContent(raw)
	if err != nil || len(blob) == 0 {
		t.Fatalf("marshal: %v", err)
	}
	back, err := p.UnmarshalRawContent(blob)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rb := back.(*anthropicRawBlocks)
	if len(rb.Blocks) != 2 || string(rb.Blocks[1]) != string(raw.Blocks[1]) {
		t.Fatalf("round-trip lost blocks: %v", rb.Blocks)
	}
}

// Replay is gated on the reference protocol being active in THIS request: a
// deferred tool in the tools array replays the captured blocks; without one
// (defer_mode normal, or a resumed session on a provider whose endpoint never
// saw defer_loading) the history degrades to its text — no endpoint guessing.
func TestAnthropicServerBlockReplay(t *testing.T) {
	blocks := []json.RawMessage{
		json.RawMessage(`{"type":"server_tool_use","id":"srv_1","name":"tool_search_tool_regex","input":{"pattern":"weather"}}`),
		json.RawMessage(`{"type":"tool_search_tool_result","tool_use_id":"srv_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"atmos_query"}]}}`),
	}
	msgs := []Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", Content: "found atmos_query", RawContent: &anthropicRawBlocks{Blocks: blocks}},
		{Role: "user", Content: "use it"},
	}
	deferredTool := ToolDef{Name: "atmos_query", Description: "d", InputSchema: map[string]any{"type": "object"}, Deferred: true}
	plainTool := ToolDef{Name: "atmos_query", Description: "d", InputSchema: map[string]any{"type": "object"}}

	for _, tc := range []struct {
		name  string
		tools []ToolDef
		want  string
	}{
		{"deferred-tool-replays", []ToolDef{deferredTool}, "server_tool_use,tool_search_tool_result,text"},
		{"no-deferred-strips", []ToolDef{plainTool}, "text"},
	} {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &got)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		}))
		p := NewAnthropic("sk-test", srv.URL, "claude-sonnet-5", nil, srv.Client())
		var sink strings.Builder
		p.StreamChatWithTools(context.Background(), msgs, tc.tools, &sink, nopWC{})
		srv.Close()

		asst := got["messages"].([]any)[1].(map[string]any)
		var types []string
		for _, b := range asst["content"].([]any) {
			types = append(types, b.(map[string]any)["type"].(string))
		}
		if strings.Join(types, ",") != tc.want {
			t.Errorf("%s: content types = %v, want %v", tc.name, types, tc.want)
		}
	}
}
