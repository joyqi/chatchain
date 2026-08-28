package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joyqi/iota/provider"
)

// writeBundle fabricates a session bundle on disk (meta.json + a one-line
// messages.jsonl) with a chosen id, so locator/scope tests are deterministic.
// cwd == "" mimics an old bundle written before the field existed.
func writeBundle(t *testing.T, dir, id, cwd string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	now := time.Now().Format(time.RFC3339)
	meta, err := json.Marshal(sessionMeta{
		Version:      sessionSchemaVersion,
		ID:           id,
		CreatedAt:    now,
		UpdatedAt:    now,
		Provider:     "stub",
		Model:        "m1",
		Cwd:          cwd,
		MessageCount: 1,
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	line := `{"role":"user","content":"hi"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// bucketDir is the on-disk bucket for a project root under the test HOME.
func bucketDir(home, root string) string {
	return filepath.Join(home, ".iota", "sessions", projectsDirName, projectSlug(root))
}

func TestProjectSlug(t *testing.T) {
	cases := []struct{ root, want string }{
		{"/Users/x/proj", "-Users-x-proj"},
		{"/Users/x/proj/", "-Users-x-proj"}, // cleaned before encoding
		{"/Users/x/my-app", "-Users-x-my-app"},
		{"/", "-"},
	}
	for _, c := range cases {
		if got := projectSlug(c.root); got != c.want {
			t.Errorf("projectSlug(%q) = %q, want %q", c.root, got, c.want)
		}
	}
}

// An agent-mode writer lands in the project's bucket and records the root as
// cwd; a normal-mode writer stays flat but still records where it started.
func TestProjectSessionWriter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := &stubProvider{model: "m1"}
	root := "/work/myproj"

	sw, err := NewSessionWriter(p, nil, "", root, true)
	if err != nil {
		t.Fatalf("NewSessionWriter: %v", err)
	}
	if err := sw.AppendMessages([]provider.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw.Close()

	want := filepath.Join(bucketDir(home, root), sw.ID())
	if _, err := os.Stat(filepath.Join(want, "meta.json")); err != nil {
		t.Fatalf("bundle not in project bucket %s: %v", want, err)
	}
	sess, err := LoadSession(sw.ID(), p)
	if err != nil {
		t.Fatalf("LoadSession via locator: %v", err)
	}
	if sess.Meta.Cwd != root {
		t.Errorf("meta cwd = %q, want %q", sess.Meta.Cwd, root)
	}

	// Normal mode: flat layout, cwd recorded anyway.
	sw2, err := NewSessionWriter(p, nil, "", "/somewhere/else", false)
	if err != nil {
		t.Fatalf("NewSessionWriter (flat): %v", err)
	}
	if err := sw2.AppendMessages([]provider.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	sw2.Close()
	flat := filepath.Join(home, ".iota", "sessions", sw2.ID())
	if _, err := os.Stat(filepath.Join(flat, "meta.json")); err != nil {
		t.Fatalf("flat bundle missing at %s: %v", flat, err)
	}
	sess2, err := LoadSession(sw2.ID(), p)
	if err != nil {
		t.Fatalf("LoadSession (flat): %v", err)
	}
	if sess2.Meta.Cwd != "/somewhere/else" {
		t.Errorf("flat meta cwd = %q", sess2.Meta.Cwd)
	}
}

// The locator resolves ids in either layout, prefix matching spans both, and
// agent-mode resolution prefers the project's own bucket before going global.
func TestSessionLocatorAcrossLayouts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := &stubProvider{model: "m1"}
	base := filepath.Join(home, ".iota", "sessions")
	root := "/work/p1"

	flatID := "aaaa00000000"
	bucketID := "aaab00000000"
	writeBundle(t, filepath.Join(base, flatID), flatID, "/elsewhere")
	writeBundle(t, filepath.Join(bucketDir(home, root), bucketID), bucketID, root)

	// ResumeSession finds both layouts.
	for _, id := range []string{flatID, bucketID} {
		sw, sess, err := ResumeSession(id, p)
		if err != nil {
			t.Fatalf("ResumeSession(%s): %v", id, err)
		}
		sw.Close()
		if len(sess.Messages) != 1 {
			t.Errorf("ResumeSession(%s): %d messages, want 1", id, len(sess.Messages))
		}
	}
	if _, err := LoadFullHistory(bucketID, p); err != nil {
		t.Errorf("LoadFullHistory (bucketed): %v", err)
	}

	// A prefix with no flat match widens to the buckets.
	if id, err := ResolveSessionID("aaab", ""); err != nil || id != bucketID {
		t.Errorf("widened prefix: got %q, %v", id, err)
	}
	// Mode-first: "aaa" is ambiguous across layouts but UNIQUE within the
	// flat view, so normal mode resolves to its own session.
	if id, err := ResolveSessionID("aaa", ""); err != nil || id != flatID {
		t.Errorf("flat-first prefix: got %q, %v", id, err)
	}
	// ...and unique within the project bucket (project-first resolution).
	if id, err := ResolveSessionID("aaa", root); err != nil || id != bucketID {
		t.Errorf("project-first prefix: got %q, %v", id, err)
	}
	// An explicit id outside the bucket still resolves via the global fallback.
	if id, err := ResolveSessionID(flatID, root); err != nil || id != flatID {
		t.Errorf("global fallback: got %q, %v", id, err)
	}
	// Unknown fragments still error.
	if _, err := ResolveSessionID("zzz", root); err == nil {
		t.Error("unknown fragment: expected error")
	}
}

// The display views are mode-isolated: "" lists only flat sessions, a
// project root only its bucket. listAllSessions (the --resume resolution
// view) still merges everything, labeling bucketed entries with their
// project.
func TestListSessionsScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".iota", "sessions")
	root1, root2 := "/work/p1", "/work/p2"

	writeBundle(t, filepath.Join(base, "aaaa00000000"), "aaaa00000000", "/elsewhere")
	writeBundle(t, filepath.Join(bucketDir(home, root1), "bbbb00000000"), "bbbb00000000", root1)
	writeBundle(t, filepath.Join(bucketDir(home, root2), "cccc00000000"), "cccc00000000", root2)

	flat, err := ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions flat: %v", err)
	}
	if len(flat) != 1 || flat[0].ID != "aaaa00000000" {
		t.Fatalf("flat view must hide project buckets: %+v", flat)
	}

	all, err := listAllSessions()
	if err != nil {
		t.Fatalf("listAllSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("resolution view: %d sessions, want 3", len(all))
	}
	hints := map[string]string{}
	for _, in := range all {
		hints[in.ID] = in.Project
	}
	if hints["aaaa00000000"] != "" {
		t.Errorf("flat session got project hint %q", hints["aaaa00000000"])
	}
	if hints["bbbb00000000"] != "p1" || hints["cccc00000000"] != "p2" {
		t.Errorf("bucketed hints wrong: %v", hints)
	}

	scoped, err := ListSessions(root1)
	if err != nil {
		t.Fatalf("ListSessions scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != "bbbb00000000" {
		t.Fatalf("scoped: %+v, want just bbbb00000000", scoped)
	}

	// A project with no bucket yet is simply empty.
	if none, err := ListSessions("/work/never"); err != nil || len(none) != 0 {
		t.Errorf("empty bucket: %v, %v", none, err)
	}

	// Labels carry the hint for bucketed sessions only.
	for _, in := range all {
		if in.Project != "" && !strings.Contains(sessionLabel(in), "["+in.Project+"]") {
			t.Errorf("label missing project hint: %q", sessionLabel(in))
		}
	}
	if l := sessionLabel(SessionInfo{ID: "x", Title: "t", Model: "m"}); strings.Contains(l, "[") {
		t.Errorf("flat label grew a hint: %q", l)
	}
}

// DeleteSession removes bucketed bundles too, and can never remove the
// projects/ container itself (it has no meta.json, so the locator skips it).
func TestDeleteSessionInBucket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := "/work/p1"
	id := "bbbb00000000"
	writeBundle(t, filepath.Join(bucketDir(home, root), id), id, root)

	if err := DeleteSession(id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bucketDir(home, root), id)); !os.IsNotExist(err) {
		t.Errorf("bundle survived delete; stat err = %v", err)
	}
	if err := DeleteSession(projectsDirName); err == nil {
		t.Error("deleting the projects container should fail")
	}
	if _, err := os.Stat(filepath.Join(home, ".iota", "sessions", projectsDirName)); err != nil {
		t.Errorf("projects container gone: %v", err)
	}
}

// Old bundles (flat layout, no cwd) behave exactly as today: globally visible
// without a hint, absent from any project's scoped view, resumable by prefix.
func TestOldSessionCompat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := &stubProvider{model: "m1"}
	base := filepath.Join(home, ".iota", "sessions")
	id := "dddd00000000"
	writeBundle(t, filepath.Join(base, id), id, "")

	global, err := ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(global) != 1 || global[0].ID != id || global[0].Project != "" {
		t.Fatalf("old session listing wrong: %+v", global)
	}
	if scoped, _ := ListSessions("/work/p1"); len(scoped) != 0 {
		t.Errorf("old flat session leaked into a project scope: %+v", scoped)
	}
	if got, err := ResolveSessionID("dddd", ""); err != nil || got != id {
		t.Errorf("resolve old session: %q, %v", got, err)
	}
	sw, _, err := ResumeSession(id, p)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	sw.Close()
}

// Fresh ids never collide with a bundle in either layout.
func TestSessionIDTaken(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".iota", "sessions")
	root := "/work/p1"
	if err := os.MkdirAll(filepath.Join(base, "aaaa00000000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, projectsDirName, projectSlug(root), "bbbb00000000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !sessionIDTaken(base, "aaaa00000000") {
		t.Error("flat id not seen as taken")
	}
	if !sessionIDTaken(base, "bbbb00000000") {
		t.Error("bucketed id not seen as taken")
	}
	if sessionIDTaken(base, "cccc00000000") {
		t.Error("free id reported taken")
	}
}
