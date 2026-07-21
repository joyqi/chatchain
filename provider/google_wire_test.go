package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatchain/internal/llm"
)

// genaiContentFixture is the exact JSON google.golang.org/genai@v1.63.0
// produced for a persisted RawContent blob (captured via json.Marshal on a
// *genai.Content). Sessions saved before the SDK removal carry this shape;
// llm.GContent must keep decoding it and re-marshal without losing fields.
const genaiContentFixture = `{"parts":[{"text":"thinking...","thought":true,"thoughtSignature":"AQID"},{"text":"answer"},{"functionCall":{"id":"c1","args":{"q":1},"name":"f"}},{"inlineData":{"data":"CQ==","mimeType":"image/png"}},{"functionResponse":{"name":"f","response":{"output":"ok"}}}],"role":"model"}`

func TestGoogleRawContentBlobCompat(t *testing.T) {
	p := NewGemini("k", "", "m", nil, nil)
	v, err := p.UnmarshalRawContent([]byte(genaiContentFixture))
	if err != nil {
		t.Fatal(err)
	}
	c := v.(*llm.GContent)
	if len(c.Parts) != 5 || !c.Parts[0].Thought || string(c.Parts[0].ThoughtSignature) != "\x01\x02\x03" {
		t.Fatalf("fixture decode lost data: %+v", c)
	}
	if c.Parts[2].FunctionCall.ID != "c1" || c.Parts[3].InlineData.MimeType != "image/png" {
		t.Fatalf("fixture decode lost data: %+v", c)
	}
	// Round-trip: re-marshalled blob must be semantically identical.
	out, err := p.MarshalRawContent(c)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	json.Unmarshal(out, &a)
	json.Unmarshal([]byte(genaiContentFixture), &b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("round-trip drift:\n got %s\nwant %s", aj, bj)
	}
}

// TestGoogleGoldenRequest pins the generateContent request wire: URL forms per
// backend (models/ vs publishers/google/models/, version segments, alt=sse),
// x-goog-api-key auth, camelCase body (systemInstruction, generationConfig
// with thinkingConfig, inlineData attachments, functionDeclarations), and the
// toolCallIDs backend split.
func TestGoogleGoldenRequest(t *testing.T) {
	var path, key string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path + "?" + r.URL.RawQuery
		key = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"candidates\":[]}\n\n"))
	}))
	defer srv.Close()

	temp := 0.5
	p := NewGemini("gk", srv.URL, "gemini-2.5-pro", &temp, srv.Client())
	p.SetEffort("high")
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "look", Attachments: []Attachment{{Filename: "a.png", MimeType: "image/png", Data: []byte{9}}}},
	}
	tools := []ToolDef{{Name: "f", Description: "d", InputSchema: map[string]any{"type": "object"}}}
	_, _, _, err := p.StreamChatWithTools(context.Background(), msgs, tools, io.Discard, nopCloser{io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	if path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse" || key != "gk" {
		t.Fatalf("path=%q key=%q", path, key)
	}
	if got["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "sys" {
		t.Fatalf("systemInstruction = %v", got["systemInstruction"])
	}
	gc := got["generationConfig"].(map[string]any)
	if gc["temperature"].(float64) != 0.5 {
		t.Fatalf("generationConfig = %v", gc)
	}
	tc := gc["thinkingConfig"].(map[string]any)
	if tc["includeThoughts"] != true || tc["thinkingLevel"] != "HIGH" {
		t.Fatalf("thinkingConfig = %v", tc)
	}
	parts := got["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	blob := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if blob["mimeType"] != "image/png" || blob["data"] != "CQ==" {
		t.Fatalf("inlineData = %v", blob)
	}
	decl := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if decl["name"] != "f" || decl["parametersJsonSchema"].(map[string]any)["type"] != "object" {
		t.Fatalf("functionDeclarations = %v", decl)
	}

	// Vertex express: publisher path + v1beta1 on the default endpoint form.
	pv := NewVertexAI("vk", srv.URL, "gemini-2.5-pro", nil, srv.Client())
	pv.client.Version = "v1beta1" // custom baseURL historically flips to v1; pin the default form here
	_, _, err2 := pv.StreamChat(context.Background(), []Message{{Role: "user", Content: "q"}}, io.Discard, nopCloser{io.Discard})
	if err2 != nil {
		t.Fatal(err2)
	}
	if path != "/v1beta1/publishers/google/models/gemini-2.5-pro:streamGenerateContent?alt=sse" {
		t.Fatalf("vertex path = %q", path)
	}
}

// TestGoogleStreamTranscript pins streaming consumption: thought parts to the
// reasoning writer (closed before first content), text to the content writer,
// function calls with generated IDs when absent, usage from usageMetadata, and
// the raw model Content (with thoughtSignature) captured for the next round.
func TestGoogleStreamTranscript(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hm","thought":true,"thoughtSignature":"AQID"}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Answer."},{}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewGemini("k", srv.URL, "m", nil, srv.Client())
	var content, reasoning strings.Builder
	closed := false
	full, think, calls, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "q"}}, []ToolDef{{Name: "lookup"}},
		&content, writeCloserFunc{&reasoning, func() { closed = true }})
	if err != nil {
		t.Fatal(err)
	}
	if think != "hm" || !closed || full != "Answer." {
		t.Fatalf("think=%q closed=%v full=%q", think, closed, full)
	}
	if len(calls) != 1 || calls[0].Name != "lookup" || calls[0].ID == "" || calls[0].Arguments["q"] != "x" {
		t.Fatalf("calls = %+v", calls)
	}
	if in, out, ok := p.LastUsage(); !ok || in != 7 || out != 3 {
		t.Fatalf("usage = %d/%d/%v", in, out, ok)
	}
	raw := p.LastRawContent().(*llm.GContent)
	if len(raw.Parts) != 3 { // thought+sig, text, functionCall — empty part dropped
		t.Fatalf("raw parts = %d: %+v", len(raw.Parts), raw)
	}
	if string(raw.Parts[0].ThoughtSignature) != "\x01\x02\x03" {
		t.Fatal("thought signature lost from raw content")
	}
}

// A generated image (inlineData output part) surfaces through LastImages,
// stays OUT of the raw replay parts (attachments own the bytes), and an
// assistant message carrying it serializes back as a model inlineData part —
// the iterative-editing round trip.
func TestGoogleImageOutput(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Here you go."},{"inlineData":{"mimeType":"image/png","data":"iVBO"}}]}}]}`,
		``,
	}, "\n")
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(transcript))
	}))
	defer srv.Close()

	p := NewGemini("k", srv.URL, "m", nil, srv.Client())
	var content strings.Builder
	full, _, calls, err := p.StreamChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "draw a cat"}}, nil,
		&content, writeCloserFunc{io.Discard, func() {}})
	if err != nil || len(calls) != 0 {
		t.Fatalf("stream: %v calls=%v", err, calls)
	}
	if full != "Here you go." {
		t.Fatalf("full = %q", full)
	}
	imgs := p.LastImages()
	if len(imgs) != 1 || imgs[0].MimeType != "image/png" || string(imgs[0].Data) != "\x89PN" {
		t.Fatalf("LastImages = %+v", imgs)
	}

	// Round trip: the assistant message with the attachment replays as a
	// model inlineData part plus its text.
	_, _, _, err = p.StreamChatWithTools(context.Background(), []Message{
		{Role: "user", Content: "draw a cat"},
		{Role: "assistant", Content: "Here you go.", Attachments: []Attachment{{MimeType: "image/png", Data: []byte{1, 2}}}},
		{Role: "user", Content: "make it blue"},
	}, nil, io.Discard, writeCloserFunc{io.Discard, func() {}})
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatal(err)
	}
	contents := req["contents"].([]any)
	model := contents[1].(map[string]any)
	parts := model["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("model parts = %v", parts)
	}
	if parts[0].(map[string]any)["text"] != "Here you go." {
		t.Fatalf("text part = %v", parts[0])
	}
	blob := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if blob["mimeType"] != "image/png" || blob["data"] != "AQI=" {
		t.Fatalf("inlineData = %v", blob)
	}
	// Per-stream reset: the second stream (same fixture, one image) reports
	// exactly ITS image — not an accumulation across streams.
	if len(p.LastImages()) != 1 {
		t.Fatalf("LastImages must reset per stream: %+v", p.LastImages())
	}
}
