package chat

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatchain/provider"
)

// escToCancelStdin must expand a lone ESC into the cancel sequence (navigate to
// the cancel sentinel + Enter), while passing escape sequences (arrow keys, SS3)
// and Ctrl+C through untouched.
func TestEscToCancelStdin(t *testing.T) {
	seq := string(escCancelSeq)
	cases := []struct{ in, want string }{
		{"\x1b", seq},            // lone ESC → cancel sequence
		{"\x03", seq},            // Ctrl+C → cancel sequence too
		{"hi\x1b", "hi" + seq},   // trailing ESC
		{"\x1b[A", "\x1b[A"},     // up arrow passthrough
		{"\x1bOP", "\x1bOP"},     // F1 (SS3) passthrough
		{"a\x1b[Bb", "a\x1b[Bb"}, // arrow mid-stream passthrough
		{"plain", "plain"},       // ordinary text unchanged
	}
	for _, c := range cases {
		r := &escToCancelStdin{r: strings.NewReader(c.in)}
		buf := make([]byte, 16) // small buffer to exercise the overflow queue
		var got string
		for {
			n, err := r.Read(buf)
			got += string(buf[:n])
			if err != nil || (n == 0 && len(r.queue) == 0) {
				break
			}
			if n == 0 {
				continue
			}
		}
		if got != c.want {
			t.Errorf("Read(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// stubProvider implements provider.Provider + provider.RawContentProvider for
// exercising the session round-trip without a real backend.
type stubProvider struct{ model string }

func (s *stubProvider) ListModels(ctx context.Context) ([]string, error) { return []string{"m1"}, nil }
func (s *stubProvider) Chat(ctx context.Context, msgs []provider.Message) (string, error) {
	return "SUMMARY", nil
}
func (s *stubProvider) StreamChat(ctx context.Context, msgs []provider.Message, w io.Writer, r io.WriteCloser) (string, string, error) {
	r.Close()
	return "", "", nil
}
func (s *stubProvider) Type() string                            { return "stub" }
func (s *stubProvider) Model() string                           { return s.model }
func (s *stubProvider) SetModel(m string)                       { s.model = m }
func (s *stubProvider) LastRawContent() any                     { return nil }
func (s *stubProvider) MarshalRawContent(v any) ([]byte, error) { return json.Marshal(v) }
func (s *stubProvider) UnmarshalRawContent(d []byte) (any, error) {
	var m map[string]any
	err := json.Unmarshal(d, &m)
	return m, err
}

func TestSessionRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, err := NewSessionWriter(p, nil, "")
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	raw := map[string]any{"sig": "abc"}
	msgs := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi", Attachments: []provider.Attachment{{Filename: "a.txt", MimeType: "text/plain", Data: []byte("hello")}}},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "tool", Arguments: map[string]any{"x": float64(1)}}}, RawContent: raw},
		{Role: "tool", Content: "result", ToolCallID: "c1", ToolCallName: "tool"},
		{Role: "assistant", Content: "done"},
	}
	if err := sw.AppendMessages(msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sess, err := LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(sess.Messages) != len(msgs) {
		t.Fatalf("message count: got %d want %d", len(sess.Messages), len(msgs))
	}

	// Attachment bytes restored.
	u := sess.Messages[1]
	if len(u.Attachments) != 1 || string(u.Attachments[0].Data) != "hello" {
		t.Errorf("attachment not restored: %+v", u.Attachments)
	}

	// Tool call preserved.
	a := sess.Messages[2]
	if len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "c1" || a.ToolCalls[0].Name != "tool" {
		t.Errorf("tool call not restored: %+v", a.ToolCalls)
	}
	if a.ToolCalls[0].Arguments["x"] != float64(1) {
		t.Errorf("tool args not restored: %+v", a.ToolCalls[0].Arguments)
	}
	// RawContent restored (same provider type).
	rc, ok := a.RawContent.(map[string]any)
	if !ok || rc["sig"] != "abc" {
		t.Errorf("raw content not restored: %#v", a.RawContent)
	}

	// Tool result fields preserved.
	tr := sess.Messages[3]
	if tr.Role != "tool" || tr.ToolCallID != "c1" || tr.Content != "result" {
		t.Errorf("tool result not restored: %+v", tr)
	}

	// Listing reports the session with the right message count.
	infos, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].MessageCount != len(msgs) {
		t.Fatalf("ListSessions mismatch: %+v", infos)
	}
}

// Loading a session under a different provider type drops the opaque raw blob.
func TestRawContentDroppedOnProviderMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, _ := NewSessionWriter(p, nil, "")
	id := sw.ID()
	sw.AppendMessages([]provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "t"}}, RawContent: map[string]any{"sig": "abc"}},
	})
	sw.Close()

	other := &stubProvider{model: "m1"}
	// Force a different Type() by wrapping.
	sess, err := LoadSession(id, otherType{other})
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Messages[0].RawContent != nil {
		t.Errorf("expected raw dropped on provider mismatch, got %#v", sess.Messages[0].RawContent)
	}
}

type otherType struct{ *stubProvider }

func (otherType) Type() string { return "different" }

// A session must not touch disk until a real message is appended — commands-only
// sessions (SetModel/SetTitle, no turn) leave nothing behind.
func TestLazySessionCreation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := &stubProvider{model: "m1"}

	sw, err := NewSessionWriter(p, nil, "")
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	// In-memory updates (e.g. /model before any turn) must not create the bundle.
	sw.SetModel("m2")
	sw.SetTitle("draft")
	if infos, _ := ListSessions(); len(infos) != 0 {
		t.Fatalf("expected no sessions on disk before any append, got %d", len(infos))
	}
	if _, err := os.Stat(filepath.Join(home, ".chatchain", "sessions", id)); !os.IsNotExist(err) {
		t.Fatalf("session dir should not exist before append; stat err = %v", err)
	}

	// First real append materializes the bundle (with the pending model/title).
	if err := sw.AppendMessages([]provider.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	infos, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("expected 1 session after append: %+v", infos)
	}
	if infos[0].Model != "m2" || infos[0].Title != "draft" {
		t.Errorf("pending model/title not flushed on create: model=%q title=%q", infos[0].Model, infos[0].Title)
	}
}
