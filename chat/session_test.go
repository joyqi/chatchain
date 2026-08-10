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

	sw, err := NewSessionWriter(p, nil, "", "", false)
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
	infos, err := ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].MessageCount != len(msgs) {
		t.Fatalf("ListSessions mismatch: %+v", infos)
	}
}

// Usage rides the assistant message to disk, and a reload sums the WHOLE log
// — the rounds a compaction superseded and the compaction pass itself
// included, since those were paid for — so a resumed session's cumulative
// ↑/↓ figures continue instead of restarting at zero.
func TestSessionUsageRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}
	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()
	if err := sw.AppendMessages([]provider.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1", Usage: &provider.Usage{Input: 1000, Output: 200}},
	}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	// The summary pass is a billed call of its own; the marker carries it.
	if err := sw.AppendCompaction("SUMMARY", 0, &provider.Usage{Input: 500, Output: 50}); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := sw.AppendMessages([]provider.Message{
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2", Usage: &provider.Usage{Input: 1500, Output: 300}},
	}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	want := provider.Usage{Input: 3000, Output: 550}
	if got := sw.Usage(); got != want {
		t.Errorf("writer totals = %+v, want %+v", got, want)
	}

	w2, sess, err := ResumeSession(id, p)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	defer w2.Close()
	if sess.Usage != want {
		t.Errorf("reloaded totals = %+v, want %+v (compacted rounds count too)", sess.Usage, want)
	}
	if got := w2.Usage(); got != want {
		t.Errorf("resumed writer totals = %+v, want %+v", got, want)
	}
	last := sess.Messages[len(sess.Messages)-1]
	if last.Usage == nil || *last.Usage != (provider.Usage{Input: 1500, Output: 300}) {
		t.Errorf("per-message usage lost in the round trip: %+v", last.Usage)
	}

	// Sessions written before usage was recorded contribute nothing rather
	// than breaking the load.
	if len(sess.Messages) > 0 && sess.Messages[0].Usage != nil {
		t.Errorf("user message carries usage it never had: %+v", sess.Messages[0].Usage)
	}
}

// The Interrupted flag on an assistant message survives AppendMessages →
// loadLog, so a resumed session replays the partial reply as ordinary history.
func TestInterruptedFlagRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	msgs := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "partial reply", Reasoning: "partial thinking", Interrupted: true},
	}
	if err := sw.AppendMessages(msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	sess, err := LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("message count: got %d want 2", len(sess.Messages))
	}
	a := sess.Messages[1]
	if !a.Interrupted {
		t.Error("interrupted flag not restored")
	}
	if a.Content != "partial reply" || a.Reasoning != "partial thinking" {
		t.Errorf("partial content not restored: %+v", a)
	}
	// The flag stays off for ordinary messages (omitempty both ways).
	if sess.Messages[0].Interrupted {
		t.Error("interrupted flag leaked onto the user message")
	}
}

// Tuning metadata (temperature, effort, context window) survives a meta
// round-trip: setters before the bundle exists are flushed by the first append,
// and setters on a resumed (already-created) session write through immediately.
func TestSessionMetaTuningRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	temp := 0.7
	sw.SetTemperature(&temp)
	sw.SetEffort("high")
	sw.SetContextWindow(200_000)
	if err := sw.AppendMessages([]provider.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	sess, err := LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Meta.Temperature == nil || *sess.Meta.Temperature != 0.7 {
		t.Errorf("temperature not persisted: %v", sess.Meta.Temperature)
	}
	if sess.Meta.Effort != "high" {
		t.Errorf("effort not persisted: %q", sess.Meta.Effort)
	}
	if sess.Meta.ContextWindow != 200_000 {
		t.Errorf("context window not persisted: %d", sess.Meta.ContextWindow)
	}

	// Update the knobs on a resumed session; nil temperature drops the field.
	sw2, _, err := ResumeSession(id, p)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	sw2.SetTemperature(nil)
	sw2.SetEffort("max")
	sw2.SetContextWindow(32_000)
	sw2.Close()

	sess, err = LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession after update: %v", err)
	}
	if sess.Meta.Temperature != nil {
		t.Errorf("temperature not cleared: %v", *sess.Meta.Temperature)
	}
	if sess.Meta.Effort != "max" || sess.Meta.ContextWindow != 32_000 {
		t.Errorf("updated tuning not persisted: effort=%q window=%d", sess.Meta.Effort, sess.Meta.ContextWindow)
	}
}

// tunableStub is a stubProvider that also implements provider.Tunable, for
// exercising the resume-time tuning replay without a real backend.
type tunableStub struct {
	stubProvider
	temp   *float64
	effort string
}

func (s *tunableStub) SetTemperature(t *float64) { s.temp = t }
func (s *tunableStub) Temperature() *float64     { return s.temp }
func (s *tunableStub) SetEffort(level string)    { s.effort = level }
func (s *tunableStub) Effort() string            { return s.effort }

func TestApplySessionTuning(t *testing.T) {
	temp := 0.5
	meta := sessionMeta{Provider: "stub", Temperature: &temp, Effort: "high", ContextWindow: 200_000}

	t.Run("applies all recorded knobs", func(t *testing.T) {
		p := &tunableStub{}
		window := 0
		ApplySessionTuning(&Session{Meta: meta}, p, false, false, func(n int) { window = n })
		if p.temp == nil || *p.temp != 0.5 {
			t.Errorf("temperature not applied: %v", p.temp)
		}
		if p.effort != "high" {
			t.Errorf("effort not applied: %q", p.effort)
		}
		if window != 200_000 {
			t.Errorf("window not applied: %d", window)
		}
	})

	t.Run("explicit temperature flag wins", func(t *testing.T) {
		cur := 0.9
		p := &tunableStub{temp: &cur}
		ApplySessionTuning(&Session{Meta: meta}, p, true, false, nil)
		if p.temp == nil || *p.temp != 0.9 {
			t.Errorf("flag temperature overridden: %v", p.temp)
		}
		if p.effort != "high" {
			t.Errorf("effort should still apply: %q", p.effort)
		}
	})

	t.Run("explicit window flag wins", func(t *testing.T) {
		p := &tunableStub{}
		window := 0
		ApplySessionTuning(&Session{Meta: meta}, p, false, true, func(n int) { window = n })
		if window != 0 {
			t.Errorf("flag window overridden: %d", window)
		}
	})

	t.Run("provider type mismatch applies nothing", func(t *testing.T) {
		p := &tunableStub{}
		window := 0
		other := meta
		other.Provider = "different"
		ApplySessionTuning(&Session{Meta: other}, p, false, false, func(n int) { window = n })
		if p.temp != nil || p.effort != "" || window != 0 {
			t.Errorf("tuning applied across provider types: temp=%v effort=%q window=%d", p.temp, p.effort, window)
		}
	})

	t.Run("unset values leave current tuning", func(t *testing.T) {
		cur := 0.9
		p := &tunableStub{temp: &cur, effort: "low"}
		window := 0
		ApplySessionTuning(&Session{Meta: sessionMeta{Provider: "stub"}}, p, false, false, func(n int) { window = n })
		if p.temp == nil || *p.temp != 0.9 || p.effort != "low" || window != 0 {
			t.Errorf("unset meta clobbered current tuning: temp=%v effort=%q window=%d", p.temp, p.effort, window)
		}
	})
}

// Loading a session under a different provider type drops the opaque raw blob.
func TestRawContentDroppedOnProviderMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, _ := NewSessionWriter(p, nil, "", "", false)
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

	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	// In-memory updates (e.g. /model before any turn) must not create the bundle.
	sw.SetModel("m2")
	sw.SetTitle("draft")
	if infos, _ := ListSessions(""); len(infos) != 0 {
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

	infos, err := ListSessions("")
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

// LoadFullHistory returns every conversation message on disk — including the
// rounds a compaction marker hides from the loadLog view — and skips the
// marker itself.
func TestLoadFullHistoryIgnoresCompaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	id := sw.ID()

	round1 := []provider.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	round2 := []provider.Message{
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}
	if err := sw.AppendMessages(round1); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if err := sw.AppendCompaction("SUMMARY", 0, nil); err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	if err := sw.AppendMessages(round2); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	view, err := LoadSession(id, p)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	full, err := LoadFullHistory(id, p)
	if err != nil {
		t.Fatalf("LoadFullHistory: %v", err)
	}
	if len(full) != 4 {
		t.Fatalf("full history: got %d messages, want 4", len(full))
	}
	if len(full) <= len(view.Messages) {
		t.Fatalf("full history (%d) should exceed the compacted view (%d)", len(full), len(view.Messages))
	}
	// The pre-compaction round is intact and unweaved (no summary preamble).
	if full[0].Content != "first question" || full[1].Content != "first answer" {
		t.Errorf("pre-compaction round not restored: %+v", full[:2])
	}
	// The compaction marker itself is skipped.
	for _, m := range full {
		if m.Role == "compaction" || strings.Contains(m.Content, "SUMMARY") {
			t.Errorf("compaction marker leaked into full history: %+v", m)
		}
	}
}

func TestNewSessionID(t *testing.T) {
	base := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newSessionID(base)
		if len(id) != sessionIDLength {
			t.Fatalf("id %q length = %d, want %d", id, len(id), sessionIDLength)
		}
		for _, r := range id {
			if !strings.ContainsRune(sessionIDAlphabet, r) {
				t.Fatalf("id %q contains %q outside the alphabet", id, r)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q in a small batch", id)
		}
		seen[id] = true
	}
}

func TestResolveSessionID(t *testing.T) {
	infos := []SessionInfo{
		{ID: "k7qz3xv9m2ht"},
		{ID: "k7ab00000000"},
		{ID: "01JZXK7QZTXA9WBGVE3M8YC5DN"}, // legacy ULID (uppercase)
	}

	// Exact match wins.
	if id, err := resolveSessionID(infos, "k7qz3xv9m2ht"); err != nil || id != "k7qz3xv9m2ht" {
		t.Fatalf("exact: got %q, %v", id, err)
	}
	// Unique prefix resolves.
	if id, err := resolveSessionID(infos, "k7q"); err != nil || id != "k7qz3xv9m2ht" {
		t.Fatalf("unique prefix: got %q, %v", id, err)
	}
	// Case-insensitive prefix matches legacy ULIDs.
	if id, err := resolveSessionID(infos, "01jzxk"); err != nil || id != "01JZXK7QZTXA9WBGVE3M8YC5DN" {
		t.Fatalf("ULID prefix: got %q, %v", id, err)
	}
	// Ambiguous prefix errors and names the candidates.
	if _, err := resolveSessionID(infos, "k7"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous: got %v, want ambiguous error", err)
	}
	// Unknown fragment errors.
	if _, err := resolveSessionID(infos, "zzz"); err == nil {
		t.Fatal("unknown: got nil error")
	}
}

// TestDeferredSaveBacklog mirrors the /save flow for an ephemeral session:
// the writer is minted only when the user saves, the whole accumulated
// backlog lands in one append, and the session resumes losslessly with the
// user's chosen title.
func TestDeferredSaveBacklog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &stubProvider{model: "m1"}

	// Several turns accumulate in memory with NO writer (sw == nil: every
	// persistTurn was a no-op, the watermark never moved).
	backlog := []provider.Message{
		{Role: "user", Content: "explore this"},
		{Role: "assistant", Content: "sure"},
		{Role: "user", Content: "turned out valuable"},
		{Role: "assistant", Content: "saving then"},
	}

	// /save: mint + append everything since watermark 0 + custom title.
	sw, err := NewSessionWriter(p, nil, "", "", false)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	if err := sw.AppendMessages(backlog); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.SetTitle("keeper")
	sw.SetContextWindow(128000)
	id := sw.ID()
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, sess, err := ResumeSession(id, p)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if len(sess.Messages) != len(backlog) {
		t.Fatalf("resumed %d messages, want %d", len(sess.Messages), len(backlog))
	}
	if sess.Messages[3].Content != "saving then" {
		t.Fatalf("backlog order lost: %+v", sess.Messages[3])
	}
	if sess.Meta.Title != "keeper" {
		t.Fatalf("title = %q, want keeper", sess.Meta.Title)
	}
}

// imageGenStub is a stubProvider that also implements ImageGenTunable, for
// the imagen-session tuning replay.
type imageGenStub struct {
	stubProvider
	gen provider.ImageGenParams
}

func (s *imageGenStub) SetImageGenParams(g provider.ImageGenParams) { s.gen = g }
func (s *imageGenStub) ImageGenParams() provider.ImageGenParams     { return s.gen }
func (s *imageGenStub) ImageGenOptions() provider.ImageGenOptions   { return provider.ImageGenOptions{} }

// Recorded image-generation params replay on resume; an empty meta leaves the
// startup (config) values alone.
func TestApplySessionTuningImageParams(t *testing.T) {
	meta := sessionMeta{Provider: "stub", AspectRatio: "3:2", NegativePrompt: "blurry"}
	p := &imageGenStub{}
	ApplySessionTuning(&Session{Meta: meta}, p, false, false, nil)
	if p.gen.AspectRatio != "3:2" || p.gen.NegativePrompt != "blurry" || p.gen.ImageSize != "" {
		t.Fatalf("image params not applied: %+v", p.gen)
	}

	config := provider.ImageGenParams{AspectRatio: "1:1"}
	p2 := &imageGenStub{gen: config}
	ApplySessionTuning(&Session{Meta: sessionMeta{Provider: "stub"}}, p2, false, false, nil)
	if p2.gen != config {
		t.Fatalf("empty meta must not clobber config defaults: %+v", p2.gen)
	}
}

// The edit wire format rides along in session meta like the other knobs.
func TestApplySessionTuningJSONEdits(t *testing.T) {
	p := &jsonEditStub{}
	ApplySessionTuning(&Session{Meta: sessionMeta{Provider: "stub", JSONEdits: true}}, p, false, false, nil)
	if !p.on {
		t.Fatal("recorded json_edits not replayed")
	}
	p2 := &jsonEditStub{}
	ApplySessionTuning(&Session{Meta: sessionMeta{Provider: "stub"}}, p2, false, false, nil)
	if p2.on {
		t.Fatal("absent meta must not flip the switch")
	}
}

type jsonEditStub struct {
	stubProvider
	on bool
}

func (s *jsonEditStub) SetJSONEdits(v bool) { s.on = v }
func (s *jsonEditStub) JSONEdits() bool     { return s.on }

// Bundles written before titles were funnelled may still hold a newline;
// the picker flattens on read, because one row per session is what the
// surface's cursor arithmetic assumes.
func TestSessionLabelFlattensStoredTitle(t *testing.T) {
	label := sessionLabel(SessionInfo{Title: "draw a cat\nwith a hat", Model: "m", MessageCount: 2})
	if strings.ContainsAny(label, "\n\r\t") {
		t.Fatalf("label spans rows: %q", label)
	}
	if !strings.Contains(label, "draw a cat with a hat") {
		t.Fatalf("label = %q", label)
	}
}
