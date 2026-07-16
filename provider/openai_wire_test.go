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

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// TestOpenAIGoldenRequest pins the exact request JSON the openai provider
// emits — the wire contract OpenAI-compatible servers (deepseek, kimi) see:
// message shapes, attachment parts (image data-URL / bare-b64 file / text
// LAST), tool definitions, verbatim raw-JSON assistant replay, stream options.
func TestOpenAIGoldenRequest(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	temp := 0.7
	p := NewOpenAI("sk-test", srv.URL, "gpt-4o", &temp, srv.Client())
	p.SetEffort("high")

	rawAssistant := `{"role":"assistant","content":"prev","reasoning":"think","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "look", Attachments: []Attachment{
			{Filename: "a.png", MimeType: "image/png", Data: []byte{1}},
			{Filename: "b.pdf", MimeType: "application/pdf", Data: []byte{2}},
		}},
		{Role: "assistant", Content: "prev", ToolCalls: []ToolCall{{ID: "c1", Name: "f"}}, RawContent: rawAssistant},
		{Role: "tool", Content: "result", ToolCallID: "c1"},
	}
	tools := []ToolDef{{Name: "f", Description: "does f", InputSchema: map[string]any{"type": "object"}}}
	_, _, _, err := p.StreamChatWithTools(context.Background(), msgs, tools, io.Discard, nopCloser{io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	if auth != "Bearer sk-test" || path != "/chat/completions" {
		t.Fatalf("auth=%q path=%q", auth, path)
	}
	want := map[string]any{
		"model": "gpt-4o", "temperature": 0.7, "reasoning_effort": "high",
		"stream": true, "stream_options": map[string]any{"include_usage": true},
	}
	for k, v := range want {
		gj, _ := json.Marshal(got[k])
		wj, _ := json.Marshal(v)
		if string(gj) != string(wj) {
			t.Fatalf("%s = %s, want %s", k, gj, wj)
		}
	}
	messages := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	// user parts: image data-URL, file bare-b64+filename, text LAST
	parts := messages[1].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" || !strings.HasPrefix(img["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Fatalf("image part = %v", img)
	}
	file := parts[1].(map[string]any)["file"].(map[string]any)
	if file["filename"] != "b.pdf" || strings.HasPrefix(file["file_data"].(string), "data:") {
		t.Fatalf("file part = %v (must be bare base64)", file)
	}
	if last := parts[2].(map[string]any); last["type"] != "text" || last["text"] != "look" {
		t.Fatalf("text part not last: %v", last)
	}
	// assistant raw replay is verbatim (kimi reasoning preserved)
	rawGot, _ := json.Marshal(messages[2])
	var a, b map[string]any
	json.Unmarshal(rawGot, &a)
	json.Unmarshal([]byte(rawAssistant), &b)
	aj, _ := json.Marshal(a["reasoning"])
	if string(aj) != `"think"` {
		t.Fatalf("raw assistant replay lost fields: %s", rawGot)
	}
	// tool result
	tm := messages[3].(map[string]any)
	if tm["role"] != "tool" || tm["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %v", tm)
	}
	// tools
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools = %v", got["tools"])
	}
}

// TestOpenAIStreamTranscript replays a recorded-style SSE transcript and pins
// the consumption contract: reasoning deltas (both field spellings) reach the
// reasoning writer which closes before the first content write; interleaved
// index-keyed tool-call deltas assemble in order; usage lands from the final
// chunk; finish_reason=tool_calls yields ToolCalls plus a verbatim-replayable
// raw assistant JSON.
func TestOpenAIStreamTranscript(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning":"th"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"ink"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Let me check."}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{\"q\":"}},{"index":1,"id":"c2","function":{"name":"other","arguments":"{}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewOpenAI("k", srv.URL, "m", nil, srv.Client())
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
	if len(calls) != 2 || calls[0].ID != "c1" || calls[0].Arguments["q"] != "x" || calls[1].Name != "other" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if in, out, ok := p.LastUsage(); !ok || in != 10 || out != 5 {
		t.Fatalf("usage = %d/%d/%v", in, out, ok)
	}
	raw, _ := p.LastRawContent().(string)
	if !strings.Contains(raw, `"tool_calls"`) || !strings.Contains(raw, `"reasoning":"think"`) {
		t.Fatalf("raw assistant JSON = %s", raw)
	}
}

type writeCloserFunc struct {
	io.Writer
	onClose func()
}

func (w writeCloserFunc) Close() error { w.onClose(); return nil }
