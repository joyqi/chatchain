package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"chatchain/internal/llm"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// respCompletedSSE is a minimal clean Responses stream: no [DONE] sentinel,
// the terminal response.completed event then EOF.
const respCompletedSSE = "event: response.completed\n" +
	`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"

// TestOpenResponsesGoldenRequest pins the exact request JSON the openresponses
// provider emits: system → instructions, user attachment parts (input_image
// string data-URL + detail:auto / input_file bare-b64 / input_text LAST),
// verbatim raw-item replay skipping "message" items, function_call_output for
// tool results, FLAT tool definitions with strict:false, and the stream flag.
func TestOpenResponsesGoldenRequest(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(respCompletedSSE))
	}))
	defer srv.Close()

	temp := 0.7
	p := NewOpenResponses("sk-test", srv.URL, "gpt-5", &temp, srv.Client())
	p.SetEffort("high")
	topP := 0.9
	p.SetTopP(&topP)

	// Raw output items as persisted by a previous round (session resume path):
	// a reasoning item with provider-specific fields, a message item (must be
	// SKIPPED on replay), and the function call itself.
	reasoningItem := `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"hidden"}],"encrypted_content":"OPAQUE"}`
	rawItems := `[` + reasoningItem + `,` +
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"prev"}]},` +
		`{"id":"call_1","type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}]`
	raw, err := p.UnmarshalRawContent([]byte(rawItems))
	if err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "look", Attachments: []Attachment{
			{Filename: "a.png", MimeType: "image/png", Data: []byte{1}},
			{Filename: "b.pdf", MimeType: "application/pdf", Data: []byte{2}},
		}},
		{Role: "assistant", Content: "prev", ToolCalls: []ToolCall{{ID: "call_1", Name: "f"}}, RawContent: raw},
		{Role: "tool", Content: "result", ToolCallID: "call_1"},
	}
	tools := []ToolDef{{Name: "f", Description: "does f", InputSchema: map[string]any{"type": "object"}}}
	if _, _, _, err := p.StreamChatWithTools(context.Background(), msgs, tools, io.Discard, nopCloser{io.Discard}); err != nil {
		t.Fatal(err)
	}

	if auth != "Bearer sk-test" || path != "/responses" {
		t.Fatalf("auth=%q path=%q", auth, path)
	}
	want := map[string]any{
		"model": "gpt-5", "temperature": 0.7, "top_p": 0.9, "instructions": "sys",
		"reasoning": map[string]any{"effort": "high"}, "stream": true,
	}
	for k, v := range want {
		gj, _ := json.Marshal(got[k])
		wj, _ := json.Marshal(v)
		if string(gj) != string(wj) {
			t.Fatalf("%s = %s, want %s", k, gj, wj)
		}
	}

	// input: user message, replayed reasoning + function_call (message item
	// skipped), function_call_output. System text is NOT an input item.
	input := got["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input = %d items %v, want 4", len(input), input)
	}
	// user parts: input_image (string data URL, detail auto), input_file
	// (bare b64 + filename), input_text LAST
	user := input[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("input[0] = %v", user)
	}
	parts := user["content"].([]any)
	img := parts[0].(map[string]any)
	imgURL, isString := img["image_url"].(string)
	if img["type"] != "input_image" || !isString || !strings.HasPrefix(imgURL, "data:image/png;base64,") || img["detail"] != "auto" {
		t.Fatalf("image part = %v (image_url must be a plain data-URL string)", img)
	}
	file := parts[1].(map[string]any)
	if file["type"] != "input_file" || file["filename"] != "b.pdf" || strings.HasPrefix(file["file_data"].(string), "data:") {
		t.Fatalf("file part = %v (must be bare base64)", file)
	}
	if last := parts[2].(map[string]any); last["type"] != "input_text" || last["text"] != "look" {
		t.Fatalf("text part not last: %v", last)
	}
	// reasoning item replayed verbatim (provider-specific fields intact)
	var wantReasoning any
	json.Unmarshal([]byte(reasoningItem), &wantReasoning)
	if !reflect.DeepEqual(input[1], wantReasoning) {
		t.Fatalf("reasoning item not verbatim: %v", input[1])
	}
	// function_call replayed, message item skipped
	fc := input[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "f" {
		t.Fatalf("input[2] = %v (want the replayed function_call; message items must be skipped)", fc)
	}
	if _, hasID := fc["id"]; hasID {
		t.Fatalf("input[2] = %v: function_call id must be STRIPPED on replay (OpenAI requires fc_-prefixed ids and rejects the legacy call_-prefixed rewrite; Bedrock gateways reuse ids across parallel calls)", fc)
	}
	// tool result
	fco := input[3].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "result" {
		t.Fatalf("function_call_output = %v", fco)
	}
	// tools are FLAT (no nested "function" object), strict:false explicit
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "f" || tool["description"] != "does f" ||
		tool["parameters"].(map[string]any)["type"] != "object" || tool["strict"] != false {
		t.Fatalf("tool = %v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Fatalf("tool has a nested function object (chat-completions shape leaked): %v", tool)
	}
}

// TestOpenResponsesStreamTranscript replays a recorded-style Responses SSE
// transcript (no [DONE]; typed events; terminal response.completed) and pins
// the consumption contract: reasoning-summary deltas reach the reasoning
// writer which closes before the first content write; output_text deltas
// stream to w; function calls key by call_id even when the item id is shared
// (Bedrock collapse); every completed output item lands verbatim in the raw
// replay with function_call ids rewritten to call_id; usage comes from
// response.completed.
func TestOpenResponsesStreamTranscript(t *testing.T) {
	transcript := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"status":"in_progress"}}`,
		``,
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"th"}`,
		``,
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"ink"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"OPAQUE"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Let me "}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"check."}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check."}]}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"q\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"x\"}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"q\":\"x\"}"}`,
		``,
		// Two function_call items sharing the same item id but with distinct
		// call_ids — the Bedrock/zenmux parallel-call collapse.
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_2","name":"other","arguments":"{}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewOpenResponses("k", srv.URL, "m", nil, srv.Client())
	var content, reasoning strings.Builder
	closed := false
	closedBeforeContent := true
	rw := writeCloserFunc{&reasoning, func() { closed = true }}
	cw := writerFunc(func(b []byte) (int, error) {
		if !closed {
			closedBeforeContent = false
		}
		return content.Write(b)
	})
	full, think, calls, err := p.StreamChatWithTools(context.Background(), []Message{{Role: "user", Content: "q"}}, []ToolDef{{Name: "lookup"}}, cw, rw)
	if err != nil {
		t.Fatal(err)
	}
	if think != "think" || reasoning.String() != "think" {
		t.Fatalf("reasoning = %q / %q", think, reasoning.String())
	}
	if !closed || !closedBeforeContent {
		t.Fatalf("reasoning writer close: closed=%v beforeContent=%v", closed, closedBeforeContent)
	}
	if full != "Let me check." || content.String() != full {
		t.Fatalf("content = %q / %q", full, content.String())
	}
	if len(calls) != 2 || calls[0].ID != "call_1" || calls[0].Name != "lookup" || calls[0].Arguments["q"] != "x" ||
		calls[1].ID != "call_2" || calls[1].Name != "other" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if in, out, ok := p.LastUsage(); !ok || in != 11 || out != 7 {
		t.Fatalf("usage = %d/%d/%v", in, out, ok)
	}

	// Raw record: every completed item recorded in order, VERBATIM — id
	// hygiene happens at replay time (buildRequest strips function_call ids).
	rawJSON, err := p.MarshalRawContent(p.LastRawContent())
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(rawJSON, &items); err != nil || len(items) != 4 {
		t.Fatalf("raw items = %s (%v), want 4 items", rawJSON, err)
	}
	if items[0]["type"] != "reasoning" || items[0]["encrypted_content"] != "OPAQUE" {
		t.Fatalf("reasoning item not verbatim: %v", items[0])
	}
	if items[1]["type"] != "message" {
		t.Fatalf("message item missing from raw record: %v", items[1])
	}
	if items[2]["id"] != "fc_1" || items[2]["call_id"] != "call_1" || items[2]["name"] != "lookup" {
		t.Fatalf("function_call item not verbatim: %v", items[2])
	}
	if items[3]["id"] != "fc_1" || items[3]["call_id"] != "call_2" {
		t.Fatalf("second function_call item not verbatim (upstream id reuse must be preserved at record time): %v", items[3])
	}
}

// TestOpenResponsesStreamInlineThink pins inline-tag splitting on the
// Responses dialect: relays that don't parse reasoning (zenmux fronting a
// thinking model) leak <think> into output_text deltas — the block reaches
// the reasoning writer, which closes before the first visible write, and the
// returned content and reasoning are clean.
func TestOpenResponsesStreamInlineThink(t *testing.T) {
	transcript := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"<think>pond"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"ering</think>\n\nhi"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewOpenResponses("k", srv.URL, "m", nil, srv.Client())
	var content, reasoning strings.Builder
	closed := false
	closedBeforeContent := true
	rw := writeCloserFunc{&reasoning, func() { closed = true }}
	cw := writerFunc(func(b []byte) (int, error) {
		if !closed {
			closedBeforeContent = false
		}
		return content.Write(b)
	})
	full, think, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, cw, rw)
	if err != nil {
		t.Fatal(err)
	}
	if think != "pondering" || reasoning.String() != "pondering" {
		t.Fatalf("reasoning = %q / %q", think, reasoning.String())
	}
	if !closed || !closedBeforeContent {
		t.Fatalf("reasoning writer close: closed=%v beforeContent=%v", closed, closedBeforeContent)
	}
	if full != "hi" || content.String() != "hi" {
		t.Fatalf("content = %q / %q", full, content.String())
	}
}

// TestOpenResponsesTerminalEvents pins the terminal-event fix: response.failed,
// response.incomplete, and error events yield a structured *llm.RespFailure
// carrying the event's message/code — previously they fell through silently
// and the stream looked like a clean EOF with empty output.
func TestOpenResponsesTerminalEvents(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		wantSubs []string
		wantCode string
	}{
		{
			name:     "response.failed",
			data:     `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"model exploded"}}}`,
			wantSubs: []string{"response.failed", "model exploded", "server_error"},
			wantCode: "server_error",
		},
		{
			name:     "response.incomplete",
			data:     `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
			wantSubs: []string{"response.incomplete", "max_output_tokens"},
			wantCode: "max_output_tokens",
		},
		{
			name:     "error event",
			data:     `{"type":"error","code":"ERR_UPSTREAM","message":"bad stream","param":null,"sequence_number":1}`,
			wantSubs: []string{"error", "bad stream", "ERR_UPSTREAM"},
			wantCode: "ERR_UPSTREAM",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"))
				w.Write([]byte("data: " + tc.data + "\n\n"))
			}))
			defer srv.Close()

			p := NewOpenResponses("k", srv.URL, "m", nil, srv.Client())
			_, _, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, io.Discard, nopCloser{io.Discard})
			if err == nil {
				t.Fatal("terminal failure event surfaced no error (the old silent-EOF bug)")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, llm.ErrNoEvents) {
				t.Fatalf("err = %v, want a structured failure, not EOF/no-events", err)
			}
			var failure *llm.RespFailure
			if !errors.As(err, &failure) || failure.Code != tc.wantCode {
				t.Fatalf("err = %v, want *llm.RespFailure with code %q", err, tc.wantCode)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("err %q does not mention %q", err.Error(), sub)
				}
			}
		})
	}
}

// TestOpenResponsesChatAndModels pins the non-streaming surface: Chat POSTs
// /responses WITHOUT a stream flag and concatenates output_text parts across
// output items (SDK OutputText parity); ListModels shares GET /models with
// chat-completions and sorts ids.
func TestOpenResponsesChatAndModels(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &got)
			w.Write([]byte(`{"id":"resp_1","status":"completed","output":[` +
				`{"id":"rs_1","type":"reasoning","summary":[]},` +
				`{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hello"},{"type":"refusal","refusal":"no"},{"type":"output_text","text":" world"}]}]}`))
		case "/models":
			w.Write([]byte(`{"object":"list","data":[{"id":"b-model"},{"id":"a-model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOpenResponses("k", srv.URL, "m", nil, srv.Client())
	out, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil || out != "Hello world" {
		t.Fatalf("Chat = %q, %v", out, err)
	}
	if _, ok := got["stream"]; ok {
		t.Fatalf("non-streaming request carries a stream flag: %v", got)
	}
	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0] != "a-model" || models[1] != "b-model" {
		t.Fatalf("models = %v, %v", models, err)
	}
}

// The image switch advertises the image_generation built-in ahead of function
// tools; an image_generation_call item surfaces through LastImages with its
// b64 payload decoded, and the raw replay keeps the item WITHOUT the payload.
func TestOpenResponsesImageGeneration(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","output_format":"png","result":"` +
			base64.StdEncoding.EncodeToString([]byte{9, 8, 7}) + `"}}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		``,
	}, "\n")
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenResponses("sk", srv.URL, "gpt-5", nil, srv.Client())
	p.SetImageOutput(true)
	_, _, _, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "draw"}},
		[]ToolDef{{Name: "f", InputSchema: map[string]any{"type": "object"}}},
		io.Discard, nopCloser{io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	tools := got["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["type"] != "image_generation" {
		t.Fatalf("tools = %v (want the builtin first, then the function)", tools)
	}
	if tools[1].(map[string]any)["name"] != "f" {
		t.Fatalf("function tool mangled: %v", tools[1])
	}

	imgs := p.LastImages()
	if len(imgs) != 1 || imgs[0].MimeType != "image/png" || len(imgs[0].Data) != 3 {
		t.Fatalf("LastImages = %+v", imgs)
	}

	raw, _ := p.MarshalRawContent(p.LastRawContent())
	if strings.Contains(string(raw), "result") {
		t.Fatalf("raw replay must strip the b64 payload: %s", raw)
	}
	if !strings.Contains(string(raw), "ig_1") {
		t.Fatalf("raw replay must keep the call item id: %s", raw)
	}
}

// Progressive frames: partial_image events reach the observer decoded, the
// generating event raises the composing widget, and the declaration carries
// partial_images.
func TestOpenResponsesImagePartials(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.image_generation_call.generating"}`,
		``,
		`data: {"type":"response.image_generation_call.partial_image","partial_image_b64":"` +
			base64.StdEncoding.EncodeToString([]byte{1, 2}) + `"}`,
		``,
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","result":"` +
			base64.StdEncoding.EncodeToString([]byte{3, 4, 5}) + `"}}`,
		``,
	}, "\n")
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenResponses("sk", srv.URL, "gpt-5", nil, srv.Client())
	p.SetImageOutput(true)
	var partials [][]byte
	p.SetImagePartialObserver(func(d []byte) { partials = append(partials, d) })
	var raised []string
	p.SetToolCallObserver(func(name, delta string) { raised = append(raised, name) })

	_, _, _, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "draw"}}, nil, io.Discard, nopCloser{io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["partial_images"] != float64(1) {
		t.Fatalf("declaration = %v", tool)
	}
	if len(partials) != 1 || len(partials[0]) != 2 {
		t.Fatalf("partials = %v", partials)
	}
	if len(raised) == 0 || raised[0] != "image_generation" {
		t.Fatalf("widget not raised: %v", raised)
	}
	if len(p.LastImages()) != 1 {
		t.Fatalf("final image missing")
	}
}
