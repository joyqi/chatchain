package chat

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chatchain/internal/ui"
	"chatchain/provider"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

// Session persistence: every interactive session is stored as a directory
// bundle under ~/.chatchain/sessions/<id>/:
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
	Image         bool     `json:"image,omitempty"`
	BaseURL       string   `json:"base_url,omitempty"`
	// Cwd is where the session was started: the project root in agent mode,
	// the plain working directory otherwise. Labels and any future
	// reorganisation read it from here — meta is the source of truth, the
	// bucket path is just an index. Old sessions simply lack it.
	Cwd          string `json:"cwd,omitempty"`
	Title        string `json:"title,omitempty"`
	MessageCount int    `json:"message_count"`
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
	Interrupted  bool                `json:"interrupted,omitempty"`
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
	// Project is the short project hint shown for bucketed (agent-mode)
	// sessions in the global list; "" for flat sessions and scoped views.
	Project string
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

// projectsDirName is the sessionsDir subdirectory holding project-scoped
// buckets: agent-mode sessions live under projects/<slug>/<id>/ where <slug>
// encodes the project root. Normal-mode sessions stay flat in sessionsDir.
const projectsDirName = "projects"

// projectSlug encodes an absolute project root as a bucket directory name,
// Claude Code style: every path separator becomes '-'
// (e.g. /Users/x/proj → -Users-x-proj).
func projectSlug(root string) string {
	return strings.ReplaceAll(filepath.Clean(root), string(filepath.Separator), "-")
}

// findSessionDir locates a session bundle by exact id across both layouts:
// the flat sessionsDir first, then every projects/<slug>/ bucket. A bundle is
// recognized by its meta.json — which also keeps the projects/ container (or
// any stray directory) from masquerading as a session. Returns "" when the id
// is not on disk.
func findSessionDir(base, id string) string {
	candidates := []string{filepath.Join(base, id)}
	if buckets, err := os.ReadDir(filepath.Join(base, projectsDirName)); err == nil {
		for _, b := range buckets {
			if b.IsDir() {
				candidates = append(candidates, filepath.Join(base, projectsDirName, b.Name(), id))
			}
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
			return dir
		}
	}
	return ""
}

// sessionDir resolves a full session id to its bundle directory via
// findSessionDir, erroring when no bundle exists in either layout.
func sessionDir(id string) (string, error) {
	base, err := sessionsDir()
	if err != nil {
		return "", err
	}
	if dir := findSessionDir(base, id); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("session %s not found", id)
}

// sessionIDAlphabet is Crockford base32, lowercased: no i/l/o/u, so ids stay
// unambiguous to read and retype.
const sessionIDAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// sessionIDLength (12 chars ≈ 60 random bits) keeps ids short enough to read
// and type while making local collisions vanishingly unlikely — and creation
// double-checks against the store anyway.
const sessionIDLength = 12

// newSessionID returns a fresh random session id with no bundle under base.
// Fully random — no time prefix, so ids differ from their first character
// (older sessions used ULIDs, whose shared timestamp prefix made them hard to
// tell apart); --resume accepts any unique prefix of either form.
func newSessionID(base string) string {
	for {
		id := gonanoid.MustGenerate(sessionIDAlphabet, sessionIDLength)
		if !sessionIDTaken(base, id) {
			return id
		}
	}
}

// sessionIDTaken reports whether any directory with this id exists in either
// layout — flat or any projects/<slug>/ bucket — so ids stay unique across the
// whole store and prefix resolution never has to disambiguate by location.
func sessionIDTaken(base, id string) bool {
	if _, err := os.Stat(filepath.Join(base, id)); err == nil {
		return true
	}
	buckets, err := os.ReadDir(filepath.Join(base, projectsDirName))
	if err != nil {
		return false
	}
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, projectsDirName, b.Name(), id)); err == nil {
			return true
		}
	}
	return false
}

// errNoSessionMatch marks a fragment that matched nothing — scoped resolution
// widens to the global view on it (an ambiguous fragment, by contrast, can
// only get more ambiguous in the superset, so it is final).
var errNoSessionMatch = errors.New("no session matches")

// resolveSessionID matches a user-supplied id fragment against the known
// sessions: an exact match wins, otherwise a unique (case-insensitive) prefix;
// unknown or ambiguous fragments return a descriptive error.
func resolveSessionID(infos []SessionInfo, fragment string) (string, error) {
	var matches []string
	for _, in := range infos {
		if in.ID == fragment {
			return in.ID, nil
		}
		if strings.HasPrefix(strings.ToLower(in.ID), strings.ToLower(fragment)) {
			matches = append(matches, in.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w %q", errNoSessionMatch, fragment)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("session id %q is ambiguous: %s", fragment, strings.Join(matches, ", "))
	}
}

// ResolveSessionID resolves a --resume id fragment to a full session id
// (exact match or unique prefix; see resolveSessionID). The mode's OWN view
// is tried first — the project bucket in agent mode, the flat sessions
// otherwise — so a short prefix means "one of mine"; only when nothing there
// matches does resolution widen to every bucket, keeping an explicit id
// working from anywhere.
func ResolveSessionID(fragment, projectRoot string) (string, error) {
	scoped, err := ListSessions(projectRoot)
	if err != nil {
		return "", err
	}
	id, rerr := resolveSessionID(scoped, fragment)
	if rerr == nil {
		return id, nil
	}
	if !errors.Is(rerr, errNoSessionMatch) {
		return "", rerr // ambiguous within the mode's own view — final
	}
	all, err := listAllSessions()
	if err != nil {
		return "", err
	}
	return resolveSessionID(all, fragment)
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

	// titleMu guards meta.Title: the async first-reply title goroutine writes
	// it (SetTitle) while the main loop may read it (Title, e.g. /export).
	titleMu sync.Mutex
}

// NewSessionWriter prepares a fresh session in memory but does NOT touch disk.
// The bundle (dir + meta.json + messages.jsonl) is created lazily on the first
// AppendMessages — so a session that only runs commands and never reaches a real
// turn leaves nothing behind. cwd is recorded in meta in both modes (the
// project root in agent mode, the plain working directory otherwise; "" omits
// it); project additionally places the bundle in the cwd's projects/<slug>/
// bucket instead of the flat sessionsDir.
func NewSessionWriter(p provider.Provider, temperature *float64, baseURL, cwd string, project bool) (*SessionWriter, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	id := newSessionID(base)
	bucket := base
	if project && cwd != "" {
		bucket = filepath.Join(base, projectsDirName, projectSlug(cwd))
	}
	now := time.Now().Format(time.RFC3339)
	return &SessionWriter{
		dir: filepath.Join(bucket, id),
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
			Cwd:         cwd,
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
	dir, err := sessionDir(id)
	if err != nil {
		return nil, nil, err
	}
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
	if it, ok := p.(provider.ImageTunable); ok && it.SupportsImageOutput() && sess.Meta.Image {
		it.SetImageOutput(true)
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

// Title returns the session's current title ("" on a nil writer or before the
// first turn generates one).
func (w *SessionWriter) Title() string {
	if w == nil {
		return ""
	}
	w.titleMu.Lock()
	defer w.titleMu.Unlock()
	return w.meta.Title
}

// onDisk reports whether the session bundle exists on disk — false on a nil
// writer (--no-save) and before the lazy first append.
func (w *SessionWriter) onDisk() bool {
	return w != nil && w.created
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
		Interrupted:  msg.Interrupted,
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
	w.titleMu.Lock()
	defer w.titleMu.Unlock()
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

// ImagesDir returns the session bundle's images directory (created on
// demand): generated images live INSIDE the bundle so deleting the session
// deletes them. "" on a nil writer — the caller falls back to the global
// images directory for ephemeral sessions.
func (w *SessionWriter) ImagesDir() string {
	if w == nil {
		return ""
	}
	if err := w.ensureCreated(); err != nil {
		return ""
	}
	dir := filepath.Join(w.dir, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// SetImage records the image-generation switch in the session meta.
func (w *SessionWriter) SetImage(on bool) error {
	if w == nil {
		return nil
	}
	w.meta.Image = on
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
		Interrupted:  sm.Interrupted,
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

// scanRecords reads messages.jsonl under dir and calls fn for each intact
// record, in append order. Blank lines and corrupt/truncated lines are
// tolerated (a crash mid-write leaves a truncated trailing line). A
// scanner-level error (oversized line / I/O fault) is different: it stops the
// read partway, so it is surfaced instead of silently truncating the log.
func scanRecords(dir string, fn func(sessionMessage)) error {
	f, err := os.Open(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()

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
		fn(sm)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read session log: %w", err)
	}
	return nil
}

// loadLog reads messages.jsonl and reconstructs the derived view (Event Store):
// system + latest summary + conversation tail after the latest compaction marker.
// Returns the view and convCount (total conversation messages on disk, for the
// writer's marker indexing). Truncated/corrupt trailing lines are tolerated.
func loadLog(dir string, p provider.Provider) (view []provider.Message, convCount int, err error) {
	var system *provider.Message
	var conv []provider.Message
	var summary string
	through := 0
	hasSummary := false

	err = scanRecords(dir, func(sm sessionMessage) {
		if sm.Role == "compaction" {
			hasSummary = true
			summary = sm.Content
			through = sm.CompactedThrough
			return
		}
		m := fromSessionMessage(sm, dir, p)
		if m.Role == "system" {
			s := m
			system = &s
			return
		}
		conv = append(conv, m)
	})
	if err != nil {
		return nil, 0, err
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
	dir, err := sessionDir(id)
	if err != nil {
		return nil, err
	}
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

// LoadFullHistory reads a session's complete conversation log: every message
// record in messages.jsonl, in append order, with compaction markers skipped
// entirely — unlike loadLog there is no view-weaving, so compaction never
// hides older rounds. Used by /export, where the archive must be lossless.
// Attachments and raw content are restored exactly as in loadLog.
func LoadFullHistory(id string, p provider.Provider) ([]provider.Message, error) {
	dir, err := sessionDir(id)
	if err != nil {
		return nil, err
	}
	var msgs []provider.Message
	err = scanRecords(dir, func(sm sessionMessage) {
		if sm.Role == "compaction" {
			return
		}
		msgs = append(msgs, fromSessionMessage(sm, dir, p))
	})
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

// ListSessions returns sessions sorted by most-recently-updated first.
// The views are MODE-ISOLATED: a non-empty projectRoot lists only that
// project's bucket (agent mode), "" lists only the flat sessions (normal
// mode) — each mode sees its own kind and nothing else. Cross-bucket
// discovery exists solely for --resume id resolution (listAllSessions).
func ListSessions(projectRoot string) ([]SessionInfo, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	var infos []SessionInfo
	if projectRoot != "" {
		infos, err = listBucket(filepath.Join(base, projectsDirName, projectSlug(projectRoot)), true)
	} else {
		infos, err = listBucket(base, false)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].UpdatedAt.After(infos[j].UpdatedAt) })
	return infos, nil
}

// listAllSessions is the resolution view: flat sessions plus every
// projects/<slug>/ bucket. Only --resume id resolution consults it — an
// explicit id names one session and works from anywhere, while the display
// views stay mode-isolated.
func listAllSessions() ([]SessionInfo, error) {
	base, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	infos, err := listBucket(base, false)
	if err != nil {
		return nil, err
	}
	pdir := filepath.Join(base, projectsDirName)
	if buckets, berr := os.ReadDir(pdir); berr == nil {
		for _, b := range buckets {
			if !b.IsDir() {
				continue
			}
			more, _ := listBucket(filepath.Join(pdir, b.Name()), true)
			infos = append(infos, more...)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].UpdatedAt.After(infos[j].UpdatedAt) })
	return infos, nil
}

// listBucket reads one directory of session bundles into SessionInfos.
// project marks the entries as project-scoped, deriving their label hint. A
// missing directory is an empty bucket, not an error.
func listBucket(dir string, project bool) ([]SessionInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var infos []SessionInfo
	for _, e := range entries {
		if !e.IsDir() || e.Name() == projectsDirName {
			continue
		}
		meta, err := loadMeta(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		t, _ := time.Parse(time.RFC3339, meta.UpdatedAt)
		info := SessionInfo{
			ID:           meta.ID,
			Title:        meta.Title,
			Model:        meta.Model,
			Provider:     meta.Provider,
			UpdatedAt:    t,
			MessageCount: meta.MessageCount,
		}
		if project {
			info.Project = projectHint(meta, filepath.Base(dir))
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// projectHint is the short project name shown next to a bucketed session in
// the global list: the base name of the recorded cwd (meta is the source of
// truth), falling back to the slug's last segment for bundles without one.
func projectHint(meta sessionMeta, slug string) string {
	if meta.Cwd != "" {
		return filepath.Base(meta.Cwd)
	}
	if i := strings.LastIndex(slug, "-"); i >= 0 && i+1 < len(slug) {
		return slug[i+1:]
	}
	return slug
}

// sessionLabel is the one-line description shown in session pickers. The
// project hint stays plain text: picker rows go through width-based truncation
// that does not skip ANSI escapes, so no color here.
func sessionLabel(s SessionInfo) string {
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	label := fmt.Sprintf("%s · %s · %s · %d msgs", title, s.Model, humanizeTime(s.UpdatedAt), s.MessageCount)
	if s.Project != "" {
		label += fmt.Sprintf(" [%s]", s.Project)
	}
	return label
}

// DeleteSession removes a session bundle from disk, wherever it lives. The id
// must be a bare directory name (no path separators) so it can't escape the
// sessions dir; the locator only matches real bundles, so the projects/
// container itself can never be deleted by id.
func DeleteSession(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	dir, err := sessionDir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// PickSession lists sessions (scoped to projectRoot's bucket when non-empty;
// see ListSessions) and lets the user choose one to resume. Returns the chosen
// session ID, or "" when there are none or the user cancels.
func PickSession(projectRoot string) (string, error) {
	infos, err := ListSessions(projectRoot)
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
	r, rerr := ui.RunSurface(ui.TabbedSpec{Panels: []ui.Panel{{
		Title: "Select a session to resume", Kind: ui.PanelList, Items: labels, Height: 15,
	}}})
	if rerr != nil || r.Cancelled {
		return "", rerr // cancelled — caller stays put
	}
	return infos[r.Panels[0].Cursor].ID, nil
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
