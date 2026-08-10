package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The UNARY Chat path owns its usage figures exactly like the streaming
// paths: it reports what the response carried, and a response without a
// usage block reads as "unknown" instead of leaving the previous call's
// numbers standing. The session's cumulative accounting counts one call once,
// and /compact — the only Chat() caller in the chat loop — depends on it.
func TestUnaryChatOwnsItsUsage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withUse  string
		without  string
		newProv  func(url string, c *http.Client) Provider
		wantIn   int
		wantOut  int
		wantPath string
	}{
		{
			name:    "openai",
			withUse: `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			without: `{"choices":[{"message":{"content":"hi"}}]}`,
			newProv: func(url string, c *http.Client) Provider { return NewOpenAI("k", url, "m", nil, c) },
			wantIn:  11, wantOut: 7,
		},
		{
			name:    "anthropic",
			withUse: `{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":11,"output_tokens":7}}`,
			without: `{"content":[{"type":"text","text":"hi"}]}`,
			newProv: func(url string, c *http.Client) Provider { return NewAnthropic("k", url, "m", nil, c) },
			wantIn:  11, wantOut: 7,
		},
		{
			name:    "google",
			withUse: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7}}`,
			without: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
			newProv: func(url string, c *http.Client) Provider { return NewGemini("k", url, "m", nil, c) },
			wantIn:  11, wantOut: 7,
		},
		{
			name:    "openresponses",
			withUse: `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":11,"output_tokens":7}}`,
			without: `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`,
			newProv: func(url string, c *http.Client) Provider { return NewOpenResponses("k", url, "m", nil, c) },
			wantIn:  11, wantOut: 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.withUse
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(body))
			}))
			defer srv.Close()

			p := tc.newProv(srv.URL, srv.Client())
			if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "q"}}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			ur, ok := p.(UsageReporter)
			if !ok {
				t.Fatal("provider does not report usage")
			}
			if in, out, ok := ur.LastUsage(); !ok || in != tc.wantIn || out != tc.wantOut {
				t.Fatalf("usage = %d/%d/%v, want %d/%d/true", in, out, ok, tc.wantIn, tc.wantOut)
			}

			// A second call whose response omits usage must not keep reporting
			// the first call's figures.
			body = tc.without
			if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "q"}}); err != nil {
				t.Fatalf("Chat (no usage): %v", err)
			}
			if in, out, ok := ur.LastUsage(); ok {
				t.Fatalf("stale usage reported after a usage-less call: %d/%d/%v", in, out, ok)
			}
		})
	}
}
