package chat

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"chatchain/provider"

	"github.com/oklog/ulid/v2"
)

// Session persistence: every interactive session is stored as a directory
// bundle under ~/.chatchain/sessions/<ulid>/:
//
//	meta.json          session metadata (small, rewritten on each change)
//	messages.jsonl     one JSON message per line, append-only
//	attachments/<sha>  attachment bytes, content-addressed (deduped)
//
// The append-only jsonl keeps writes cheap and crash-safe: a process killed
// mid-turn leaves a still-loadable file (loader tolerates a truncated last line).
// See docs/design/session-format.md.

const sessionSchemaVersion = 1

// ---- on-disk DTOs ----

type sessionMeta struct {
	Version       int      `json:"v"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	Temperature   *float64 `json:"temperature,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	Effort        string   `json:"effort,omitempty"`
	BaseURL       string   `json:"base_url,omitempty"`
	Title         string   `json:"title,omitempty"`
	MessageCount  int      `json:"message_count"`
}

type sessionMessage struct {
	Role         string              `json:"role"`
	Content      string              `json:"content,omitempty"`
	Reasoning    string              `json:"reasoning,omitempty"`
	Attachments  []sessionAttachment `json:"attachments,omitempty"`
	ToolCalls    []sessionToolCall   `json:"tool_calls,omitempty"`
	ToolCallID   string              `json:"tool_call_id,omitempty"`
	ToolCallName string              `json:"tool_call_name,omitempty"`
	IsError      bool                `json:"is_error,omitempty"`
	Raw          *sessionRaw         `json:"raw,omitempty"`
	// CompactedThrough is set only on role=="compaction" markers: the number of
	// leading conversation messages (non-system, non-marker) this summary
	// supersedes. The view keeps system + summary + conv[CompactedThrough:].
	CompactedThrough int `json:"compacted_through,omitempty"`
}

type sessionToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type sessionAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime"`
	DataRef  string `json:"data_ref"` // "sha256:<hex>" → attachments/<hex>
}

type sessionRaw struct {
	Provider string          `json:"provider"` // provider type that produced this blob
	Blob     json.RawMessage `json:"blob"`
}

// SessionInfo is a lightweight summary used by the resume picker.
type SessionInfo struct {
	ID           string
	Title        string
	Model        string
	Provider     string
	UpdatedAt    time.Time
	MessageCount int
}

// Session is a fully loaded session (metadata + reconstructed messages).
type Session struct {
	Meta     sessionMeta
	Messages []provider.Message
}

// ---- paths ----

func sessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".chatchain", "sessions"), nil
}

func newSessionID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// ---- SessionWriter ----

// SessionWriter persists a live session. All methods are nil-safe so callers
// in ephemeral mode (--no-save) can hold a nil *SessionWriter and skip writes
// without guarding every call.
type SessionWriter struct {
	dir       string
	meta      sessionMeta
	f         *os.File // messages.jsonl, opened for append (nil until created)
	p         provider.Provider
	convCount int  // conversation messages appended (excludes system + compaction markers)
	created   bool // whether the on-disk bundle exists yet (lazy)
}

// NewSessionWriter prepares a fresh session in memory but does NOT touch disk.
// The bundle (dir + meta.json + messages.jsonl) is created lazily on the first
// AppendMessages — so a session that only runs commands and never reaches a real
// turn leaves nothing behind.
func NewSessionWriter(p provider.Provider, temperature *float64, baseURL string) (*SessionWriter, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	id := newSessionID()
	now := time.Now().Format(time.RFC3339)
	return &SessionWriter{
		dir: filepath.Join(base, id),
		p:   p,
		meta: sessionMeta{
			Version:     sessionSchemaVersion,
			ID:          id,
			CreatedAt:   now,
			UpdatedAt:   now,
			Provider:    p.Type(),
			Model:       p.Model(),
			Temperature: temperature,
			BaseURL:     baseURL,
		},
	}, nil
}

// ensureCreated materializes the bundle on disk on first use (lazy).
func (w *SessionWriter) ensureCreated() error {
	if w.created {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(w.dir, "attachments"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(w.dir, "messages.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.created = true
	return w.writeMeta()
}

// ResumeSession loads an existing session and opens its bundle for appending.
// Returns the writer (positioned to append, convCount seeded from the full log)
// plus the loaded session view.
func ResumeSession(id string, p provider.Provider) (*SessionWriter, *Session, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(base, id)
	meta, err := loadMeta(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read session %s: %w", id, err)
	}
	view, convCount, err := loadLog(dir, p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	w := &SessionWriter{dir: dir, meta: meta, f: f, p: p, convCount: convCount, created: true}
	return w, &Session{Meta: meta, Messages: view}, nil
}

// ApplySessionTuning replays a resumed session's persisted tuning knobs onto
// the live provider and context budget. Like the model replay, it only applies
// when the session was recorded under the current provider type. Explicit CLI
// flags win, so callers set skipTemperature/skipWindow when the corresponding
// flag was passed (effort has no flag); values the session never recorded leave
// the current tuning untouched. The context window is delivered through
// setWindow so both resume paths can route it — budget.setWindow for /session
// in chat, the launch-time window resolution for --resume in cmd.
func ApplySessionTuning(sess *Session, p provider.Provider, skipTemperature, skipWindow bool, setWindow func(int)) {
	if sess == nil || sess.Meta.Provider != p.Type() {
		return
	}
	if t, ok := p.(provider.Tunable); ok {
		if !skipTemperature && sess.Meta.Temperature != nil {
			t.SetTemperature(sess.Meta.Temperature)
		}
		if sess.Meta.Effort != "" {
			t.SetEffort(sess.Meta.Effort)
		}
	}
	if !skipWindow && sess.Meta.ContextWindow > 0 && setWindow != nil {
		setWindow(sess.Meta.ContextWindow)
	}
}

func (w *SessionWriter) ID() string {
	if w == nil {
		return ""
	}
	return w.meta.ID
}

func (w *SessionWriter) writeMeta() error {
	w.meta.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(w.meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.dir, "meta.json"), data, 0o644)
}

func (w *SessionWriter) writeAttachment(att provider.Attachment) (sessionAttachment, error) {
	sum := sha256.Sum256(att.Data)
	hexsum := hex.EncodeToString(sum[:])
	path := filepath.Join(w.dir, "attachments", hexsum)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, att.Data, 0o644); err != nil {
			return sessionAttachment{}, err
		}
	}
	return sessionAttachment{
		Filename: att.Filename,
		MimeType: att.MimeType,
		DataRef:  "sha256:" + hexsum,
	}, nil
}

func (w *SessionWriter) toSessionMessage(msg provider.Message) (sessionMessage, error) {
	sm := sessionMessage{
		Role:         msg.Role,
		Content:      msg.Content,
		Reasoning:    msg.Reasoning,
		ToolCallID:   msg.ToolCallID,
		ToolCallName: msg.ToolCallName,
		IsError:      msg.IsError,
	}
	for _, att := range msg.Attachments {
		sa, err := w.writeAttachment(att)
		if err != nil {
			return sessionMessage{}, err
		}
		sm.Attachments = append(sm.Attachments, sa)
	}
	for _, tc := range msg.ToolCalls {
		sm.ToolCalls = append(sm.ToolCalls, sessionToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	// Persist provider-specific raw content (thought signatures, reasoning
	// items) tagged with the producing provider type, so resume under the
	// same provider can restore the reasoning chain.
	if msg.RawContent != nil {
		if rc, ok := w.p.(provider.RawContentProvider); ok {
			if blob, err := rc.MarshalRawContent(msg.RawContent); err == nil && len(blob) > 0 {
				sm.Raw = &sessionRaw{Provider: w.p.Type(), Blob: json.RawMessage(blob)}
			}
		}
	}
	return sm, nil
}

// AppendMessages serializes and appends messages to messages.jsonl, fsyncs,
// and updates meta. No-op on a nil writer or empty slice.
func (w *SessionWriter) AppendMessages(msgs []provider.Message) error {
	if w == nil || len(msgs) == 0 {
		return nil
	}
	if err := w.ensureCreated(); err != nil {
		return err
	}
	for _, msg := range msgs {
		sm, err := w.toSessionMessage(msg)
		if err != nil {
			return err
		}
		line, err := json.Marshal(sm)
		if err != nil {
			return err
		}
		if _, err := w.f.Write(append(line, '\n')); err != nil {
			return err
		}
		w.meta.MessageCount++
		if msg.Role != "system" {
			w.convCount++
		}
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.writeMeta()
}

// AppendCompaction records a compaction marker: the summary plus how many leading
// conversation messages it supersedes (convCount - retainTail). The original
// messages stay in the log (Event Store); the marker drives the derived view on
// reload. No-op on a nil writer.
func (w *SessionWriter) AppendCompaction(summary string, retainTail int) error {
	if w == nil {
		return nil
	}
	if err := w.ensureCreated(); err != nil {
		return err
	}
	through := w.convCount - retainTail
	if through < 0 {
		through = 0
	}
	sm := sessionMessage{Role: "compaction", Content: summary, CompactedThrough: through}
	line, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.writeMeta()
}

// SetTitle / SetModel update meta in memory; they only touch disk once the
// bundle exists (so e.g. /model before any turn doesn't create a session). The
// pending value is flushed when ensureCreated writes meta on the first append.
func (w *SessionWriter) SetTitle(title string) error {
	if w == nil {
		return nil
	}
	w.meta.Title = title
	if !w.created {
		return nil
	}
	return w.writeMeta()
}

func (w *SessionWriter) SetModel(model string) error {
	if w == nil {
		return nil
	}
	w.meta.Model = model
	if !w.created {
		return nil
	}
	return w.writeMeta()
}

// SetTemperature / SetEffort / SetContextWindow update the session's tuning
// metadata the same way SetModel does: in memory always, on disk only once the
// bundle exists. Unset values (nil temperature, "" effort, 0 window) drop the
// field from meta.json via omitempty.
func (w *SessionWriter) SetTemperature(t *float64) error {
	if w == nil {
		return nil
	}
	w.meta.Temperature = t
	if !w.created {
		return nil
	}
	return w.writeMeta()
}

func (w *SessionWriter) SetEffort(level string) error {
	if w == nil {
		return nil
	}
	w.meta.Effort = level
	if !w.created {
		return nil
	}
	return w.writeMeta()
}

func (w *SessionWriter) SetContextWindow(n int) error {
	if w == nil {
		return nil
	}
	w.meta.ContextWindow = n
	if !w.created {
		return nil
	}
	return w.writeMeta()
}

func (w *SessionWriter) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

// ---- loading ----

func loadMeta(dir string) (sessionMeta, error) {
	var m sessionMeta
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func readAttachmentRef(dir, ref string) ([]byte, error) {
	h := strings.TrimPrefix(ref, "sha256:")
	if h == ref || h == "" {
		return nil, fmt.Errorf("bad attachment ref: %s", ref)
	}
	return os.ReadFile(filepath.Join(dir, "attachments", h))
}

func fromSessionMessage(sm sessionMessage, dir string, p provider.Provider) provider.Message {
	msg := provider.Message{
		Role:         sm.Role,
		Content:      sm.Content,
		Reasoning:    sm.Reasoning,
		ToolCallID:   sm.ToolCallID,
		ToolCallName: sm.ToolCallName,
		IsError:      sm.IsError,
	}
	for _, sa := range sm.Attachments {
		data, err := readAttachmentRef(dir, sa.DataRef)
		if err != nil {
			continue // skip unreadable attachment, keep the rest of the message
		}
		msg.Attachments = append(msg.Attachments, provider.Attachment{
			Filename: sa.Filename, MimeType: sa.MimeType, Data: data,
		})
	}
	for _, tc := range sm.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	// Restore raw content only when the loading provider produced it; the blob
	// is opaque and meaningless to a different provider type (degrade by drop).
	if sm.Raw != nil && sm.Raw.Provider == p.Type() {
		if rc, ok := p.(provider.RawContentProvider); ok {
			if v, err := rc.UnmarshalRawContent(sm.Raw.Blob); err == nil {
				msg.RawContent = v
			}
		}
	}
	return msg
}

// loadLog reads messages.jsonl and reconstructs the derived view (Event Store):
// system + latest summary + conversation tail after the latest compaction marker.
// Returns the view and convCount (total conversation messages on disk, for the
// writer's marker indexing). Truncated/corrupt trailing lines are tolerated.
func loadLog(dir string, p provider.Provider) (view []provider.Message, convCount int, err error) {
	f, err := os.Open(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var system *provider.Message
	var conv []provider.Message
	var summary string
	through := 0
	hasSummary := false

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024) // allow large lines
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var sm sessionMessage
		if err := json.Unmarshal(line, &sm); err != nil {
			continue // tolerate corrupt/incomplete line
		}
		if sm.Role == "compaction" {
			hasSummary = true
			summary = sm.Content
			through = sm.CompactedThrough
			continue
		}
		m := fromSessionMessage(sm, dir, p)
		if m.Role == "system" {
			s := m
			system = &s
			continue
		}
		conv = append(conv, m)
	}
	// A per-line json error is tolerated above (a crash mid-write leaves a
	// truncated trailing line). A scanner error (oversized line / I/O fault) is
	// different: it stops the read partway, so surface it instead of silently
	// truncating the session and miscounting convCount.
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("read session log: %w", err)
	}
	convCount = len(conv)

	if system != nil {
		view = append(view, *system)
	}
	start := 0
	if hasSummary {
		if through < 0 {
			through = 0
		}
		if through > len(conv) {
			through = len(conv)
		}
		start = through
	}
	retained := conv[start:]
	switch {
	case hasSummary && len(retained) > 0:
		first := retained[0]
		first.Content = summaryPreamble(summary) + first.Content
		view = append(view, first)
		view = append(view, retained[1:]...)
	case hasSummary:
		view = append(view, provider.Message{Role: "user", Content: summaryPreamble(summary)})
	default:
		view = append(view, retained...)
	}
	return view, convCount, nil
}

// LoadSession reads a session bundle into its in-memory view (see loadLog).
func LoadSession(id string, p provider.Provider) (*Session, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, id)
	meta, err := loadMeta(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read session %s: %w", id, err)
	}
	view, _, err := loadLog(dir, p)
	if err != nil {
		return nil, err
	}
	return &Session{Meta: meta, Messages: view}, nil
}

// ListSessions returns all sessions sorted by most-recently-updated first.
func ListSessions() ([]SessionInfo, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var infos []SessionInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := loadMeta(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		t, _ := time.Parse(time.RFC3339, meta.UpdatedAt)
		infos = append(infos, SessionInfo{
			ID:           meta.ID,
			Title:        meta.Title,
			Model:        meta.Model,
			Provider:     meta.Provider,
			UpdatedAt:    t,
			MessageCount: meta.MessageCount,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].UpdatedAt.After(infos[j].UpdatedAt) })
	return infos, nil
}

// sessionLabel is the one-line description shown in session pickers.
func sessionLabel(s SessionInfo) string {
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s · %s · %s · %d msgs", title, s.Model, humanizeTime(s.UpdatedAt), s.MessageCount)
}

// DeleteSession removes a session bundle from disk. The id must be a bare
// directory name (no path separators) so it can't escape the sessions dir.
func DeleteSession(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	base, err := sessionsDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(base, id))
}

// PickSession lists sessions and lets the user choose one to resume. Returns
// the chosen session ID, or "" when there are none or the user cancels.
func PickSession() (string, error) {
	infos, err := ListSessions()
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", nil
	}
	labels := make([]string, len(infos))
	for i, s := range infos {
		labels[i] = sessionLabel(s)
	}
	idx, ok := runSelect("Select a session to resume", labels, 15)
	if !ok {
		return "", nil // cancelled — caller stays put
	}
	return infos[idx].ID, nil
}

func humanizeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
}
