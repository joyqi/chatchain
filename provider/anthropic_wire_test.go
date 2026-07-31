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

// TestAnthropicGoldenRequest pins the exact request JSON the anthropic
// provider emits — the /v1/messages wire contract: headers (x-api-key +
// anthropic-version), system top-level, attachment blocks (base64 image /
// base64 PDF document / inlined text file, message text LAST), assistant
// tool_use replay, tool_result coalescing merged with the next user message,
// tools, max_tokens, temperature, output_config.effort, stream flag.
func TestAnthropicGoldenRequest(t *testing.T) {
	var got map[string]any
	var apiKey, version, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	temp := 0.7
	p := NewAnthropic("sk-ant-test", srv.URL, "claude-sonnet-4-6", &temp, srv.Client())
	p.SetEffort("high")
	topP := 0.9
	p.SetTopP(&topP)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "look", Attachments: []Attachment{
			{Filename: "a.png", MimeType: "image/png", Data: []byte{1}},
			{Filename: "b.pdf", MimeType: "application/pdf", Data: []byte{2}},
			{Filename: "c.txt", MimeType: "text/plain", Data: []byte("hello")},
		}},
		{Role: "assistant", Content: "using", ToolCalls: []ToolCall{{ID: "t1", Name: "f", Arguments: map[string]any{"q": "x"}}}},
		{Role: "tool", Content: "result", ToolCallID: "t1"},
		// Interrupt state 3: the next user message merges into the pending
		// tool results — one user message, tool_result blocks first.
		{Role: "user", Content: "next"},
	}
	tools := []ToolDef{{Name: "f", Description: "does f", InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []any{"q"},
	}}}
	_, _, _, err := p.StreamChatWithTools(context.Background(), msgs, tools, io.Discard, nopCloser{io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	if apiKey != "sk-ant-test" || version != "2023-06-01" || path != "/v1/messages" {
		t.Fatalf("x-api-key=%q anthropic-version=%q path=%q", apiKey, version, path)
	}
	want := map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 4096, "temperature": 0.7, "top_p": 0.9,
		"output_config": map[string]any{"effort": "high"},
		"system":        []any{map[string]any{"type": "text", "text": "sys"}},
		"stream":        true,
	}
	for k, v := range want {
		gj, _ := json.Marshal(got[k])
		wj, _ := json.Marshal(v)
		if string(gj) != string(wj) {
			t.Fatalf("%s = %s, want %s", k, gj, wj)
		}
	}
	messages := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3 (tool_result+user merged)", len(messages))
	}
	// user blocks: base64 image, base64 PDF document, inlined text file, text LAST
	user := messages[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("messages[0] = %v", user)
	}
	blocks := user["content"].([]any)
	if len(blocks) != 4 {
		t.Fatalf("user blocks = %d, want 4", len(blocks))
	}
	img := blocks[0].(map[string]any)
	imgSrc := img["source"].(map[string]any)
	if img["type"] != "image" || imgSrc["type"] != "base64" || imgSrc["media_type"] != "image/png" || imgSrc["data"] != "AQ==" {
		t.Fatalf("image block = %v", img)
	}
	doc := blocks[1].(map[string]any)
	docSrc := doc["source"].(map[string]any)
	if doc["type"] != "document" || docSrc["type"] != "base64" || docSrc["media_type"] != "application/pdf" || docSrc["data"] != "Ag==" {
		t.Fatalf("document block = %v", doc)
	}
	if txt := blocks[2].(map[string]any); txt["type"] != "text" || txt["text"] != "[File: c.txt]\nhello" {
		t.Fatalf("text-file block = %v", txt)
	}
	if last := blocks[3].(map[string]any); last["type"] != "text" || last["text"] != "look" {
		t.Fatalf("text block not last: %v", last)
	}
	// assistant: text block (non-empty content) + tool_use block
	assistant := messages[1].(map[string]any)
	ablocks := assistant["content"].([]any)
	if assistant["role"] != "assistant" || len(ablocks) != 2 {
		t.Fatalf("assistant message = %v", assistant)
	}
	if at := ablocks[0].(map[string]any); at["type"] != "text" || at["text"] != "using" {
		t.Fatalf("assistant text block = %v", at)
	}
	tu := ablocks[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "t1" || tu["name"] != "f" || tu["input"].(map[string]any)["q"] != "x" {
		t.Fatalf("tool_use block = %v", tu)
	}
	// merged user message: tool_result first (is_error always emitted), then text
	merged := messages[2].(map[string]any)
	mblocks := merged["content"].([]any)
	if merged["role"] != "user" || len(mblocks) != 2 {
		t.Fatalf("merged message = %v", merged)
	}
	tr := mblocks[0].(map[string]any)
	trContent := tr["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "t1" || tr["is_error"] != false || trContent["text"] != "result" {
		t.Fatalf("tool_result block = %v", tr)
	}
	if mt := mblocks[1].(map[string]any); mt["type"] != "text" || mt["text"] != "next" {
		t.Fatalf("merged text block = %v", mt)
	}
	// tools: name + description + input_schema with type/properties/required
	tool := got["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "f" || tool["description"] != "does f" ||
		schema["type"] != "object" ||
		schema["properties"].(map[string]any)["q"].(map[string]any)["type"] != "string" ||
		schema["required"].([]any)[0] != "q" {
		t.Fatalf("tools = %v", got["tools"])
	}
}

// TestAnthropicStreamTranscript replays a recorded-style SSE transcript and
// pins the consumption contract: thinking deltas reach the reasoning writer
// which closes before the first content write; content blocks accumulate BY
// INDEX so INTERLEAVED parallel tool_use input_json_deltas (and out-of-order
// content_block_stop events) assemble correctly in index order — the proof of
// the fix over the old single-accumulator scheme; usage lands from
// message_start (input) + message_delta (output); stop_reason=tool_use yields
// ToolCalls.
func TestAnthropicStreamTranscript(t *testing.T) {
	transcript := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me check."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"lookup"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"t2","name":"other"}}`,
		``,
		// Interleaved: index 3's delta arrives between index 2's two fragments.
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
		``,
		// Stops arrive out of index order.
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":3}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewAnthropic("k", srv.URL, "m", nil, srv.Client())
	var content, reasoning strings.Builder
	closed := false
	rw := writeCloserFunc{&reasoning, func() { closed = true }}
	full, think, calls, err := p.StreamChatWithTools(context.Background(), []Message{{Role: "user", Content: "q"}}, []ToolDef{{Name: "lookup"}}, &content, rw)
	if err != nil {
		t.Fatal(err)
	}
	if think != "think" || reasoning.String() != "think" {
		t.Fatalf("reasoning = %q / %q", think, reasoning.String())
	}
	if !closed {
		t.Fatal("reasoning writer never closed")
	}
	if full != "Let me check." || content.String() != full {
		t.Fatalf("content = %q", full)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", calls)
	}
	if calls[0].ID != "t1" || calls[0].Name != "lookup" || calls[0].Arguments["q"] != "x" {
		t.Fatalf("interleaved tool call corrupted: %+v", calls[0])
	}
	if calls[1].ID != "t2" || calls[1].Name != "other" || len(calls[1].Arguments) != 0 {
		t.Fatalf("tool call 1 = %+v", calls[1])
	}
	if in, out, ok := p.LastUsage(); !ok || in != 11 || out != 7 {
		t.Fatalf("usage = %d/%d/%v", in, out, ok)
	}
}

// TestAnthropicStreamErrorEvent pins the in-band `error` SSE event: it ends
// the stream with a structured error carrying the API error envelope (e.g.
// overloaded_error arrives on a 200 stream, not as HTTP 529).
func TestAnthropicStreamErrorEvent(t *testing.T) {
	transcript := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewAnthropic("k", srv.URL, "m", nil, srv.Client())
	_, _, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, io.Discard, nopCloser{io.Discard})
	if err == nil {
		t.Fatal("expected stream error")
	}
	if !strings.Contains(err.Error(), "received error while streaming") ||
		!strings.Contains(err.Error(), "overloaded_error") ||
		!strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("err = %v", err)
	}
}

// TestAnthropicModelsPagination pins GET /v1/models pagination: page forward
// with after_id=<last_id> until has_more=false, IDs collected across pages
// and sorted.
func TestAnthropicModelsPagination(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("headers = %v", r.Header)
		}
		requests = append(requests, r.URL.RequestURI())
		switch r.URL.Query().Get("after_id") {
		case "":
			io.WriteString(w, `{"data":[{"id":"claude-b"},{"id":"claude-a"}],"has_more":true,"first_id":"claude-b","last_id":"claude-a"}`)
		case "claude-a":
			io.WriteString(w, `{"data":[{"id":"claude-c"}],"has_more":false,"first_id":"claude-c","last_id":"claude-c"}`)
		default:
			t.Errorf("unexpected after_id %q", r.URL.Query().Get("after_id"))
			io.WriteString(w, `{"data":[],"has_more":false}`)
		}
	}))
	defer srv.Close()

	p := NewAnthropic("k", srv.URL, "m", nil, srv.Client())
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1] != "/v1/models?after_id=claude-a" {
		t.Fatalf("requests = %v", requests)
	}
	if len(models) != 3 || models[0] != "claude-a" || models[1] != "claude-b" || models[2] != "claude-c" {
		t.Fatalf("models = %v", models)
	}
}
