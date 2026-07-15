package provider

import (
	"testing"

	"google.golang.org/genai"
)

func googleForTest(toolCallIDs bool) *GoogleProvider {
	// buildContents never touches the client, so a clientless value is fine.
	return &GoogleProvider{
		baseProvider: baseProvider{providerType: "test", model: "test-model"},
		toolCallIDs:  toolCallIDs,
	}
}

// Regression: a streamed response can trail a zero-value part; replaying it in
// history makes Vertex AI reject the request with 400 "required oneof field
// 'data' must have one initialized field". Data-less parts must be dropped
// both from fresh stream output and from raw content persisted before the
// filter existed.
func TestBuildContentsSanitizesRawContent(t *testing.T) {
	raw := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{
				FunctionCall:     &genai.FunctionCall{Name: "get_news", Args: map[string]any{"url": "https://example.com"}},
				ThoughtSignature: []byte("sig"),
			},
			{}, // zero-value part, marshals to {}
		},
	}

	p := googleForTest(false)
	contents, _ := p.buildContents([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", RawContent: raw},
	})

	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}
	model := contents[1]
	if len(model.Parts) != 1 {
		t.Fatalf("expected empty part to be dropped, got %d parts", len(model.Parts))
	}
	if model.Parts[0].FunctionCall == nil || string(model.Parts[0].ThoughtSignature) != "sig" {
		t.Fatalf("surviving part lost its data: %+v", model.Parts[0])
	}
	// The original raw content must stay intact for session persistence.
	if len(raw.Parts) != 2 {
		t.Fatalf("sanitize mutated the original raw content: %d parts", len(raw.Parts))
	}
}

func TestSanitizeContentAllPartsEmpty(t *testing.T) {
	if c := sanitizeContent(&genai.Content{Role: "model", Parts: []*genai.Part{{}, {}}}); c != nil {
		t.Fatalf("expected nil for all-empty content, got %+v", c)
	}
}

// The Gemini Developer API accepts FunctionCall/FunctionResponse IDs; Vertex
// AI does not. The flag decides whether IDs make it into the request.
func TestBuildContentsToolCallIDs(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "f", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "call_1", ToolCallName: "f", Content: "ok"},
	}

	for _, tc := range []struct {
		toolCallIDs bool
		wantID      string
	}{
		{toolCallIDs: true, wantID: "call_1"},
		{toolCallIDs: false, wantID: ""},
	} {
		contents, _ := googleForTest(tc.toolCallIDs).buildContents(messages)
		if len(contents) != 3 {
			t.Fatalf("toolCallIDs=%v: expected 3 contents, got %d", tc.toolCallIDs, len(contents))
		}
		fc := contents[1].Parts[0].FunctionCall
		if fc == nil {
			t.Fatalf("toolCallIDs=%v: missing FunctionCall part", tc.toolCallIDs)
		}
		if fc.ID != tc.wantID {
			t.Errorf("toolCallIDs=%v: FunctionCall ID = %q, want %q", tc.toolCallIDs, fc.ID, tc.wantID)
		}
		fr := contents[2].Parts[0].FunctionResponse
		if fr == nil {
			t.Fatalf("toolCallIDs=%v: missing FunctionResponse part", tc.toolCallIDs)
		}
		if fr.ID != tc.wantID {
			t.Errorf("toolCallIDs=%v: FunctionResponse ID = %q, want %q", tc.toolCallIDs, fr.ID, tc.wantID)
		}
	}
}
