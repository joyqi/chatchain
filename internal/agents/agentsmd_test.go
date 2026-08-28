package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joyqi/iota/provider"
)

// writeAgents writes dir/AGENTS.md (creating dir), returning the file path.
func writeAgents(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, agentsFileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectRoot(t *testing.T) {
	base := t.TempDir()

	// Normal checkout: .git is a directory; found from a nested subdir.
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(sub); got != repo {
		t.Errorf("ProjectRoot(%s) = %s, want %s", sub, got, repo)
	}

	// Linked worktree: .git is a file, not a directory.
	wt := filepath.Join(base, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(wt); got != wt {
		t.Errorf("ProjectRoot(worktree) = %s, want %s", wt, got)
	}

	// No repository anywhere up the tree: fall back to cwd itself.
	plain := filepath.Join(base, "plain", "deep")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(plain); got != plain {
		t.Errorf("ProjectRoot(no repo) = %s, want %s", plain, got)
	}
}

func TestLoadAgentsChain(t *testing.T) {
	root := t.TempDir()
	mid := filepath.Join(root, "a")
	leaf := filepath.Join(mid, "b")

	rootFile := writeAgents(t, root, "ROOT\n")
	leafFile := writeAgents(t, leaf, "LEAF")
	// mid has no AGENTS.md: skipped, not an error.

	chain := loadAgentsChain(root, leaf)
	if chain.Content != "ROOT\n\nLEAF" {
		t.Errorf("Content = %q, want root-first blank-line join", chain.Content)
	}
	if want := []string{rootFile, leafFile}; len(chain.Files) != 2 || chain.Files[0] != want[0] || chain.Files[1] != want[1] {
		t.Errorf("Files = %v, want %v", chain.Files, want)
	}

	// mid gains its own file: exactly one per directory, nearer files later.
	writeAgents(t, mid, "MID")
	chain = loadAgentsChain(root, leaf)
	if chain.Content != "ROOT\n\nMID\n\nLEAF" {
		t.Errorf("Content = %q, want ROOT/MID/LEAF in order", chain.Content)
	}

	// cwd == root: only the root file participates.
	chain = loadAgentsChain(root, root)
	if chain.Content != "ROOT" {
		t.Errorf("Content(cwd=root) = %q, want %q", chain.Content, "ROOT")
	}

	// cwd outside the root degrades to the root alone.
	outside := t.TempDir()
	chain = loadAgentsChain(root, outside)
	if chain.Content != "ROOT" || len(chain.Files) != 1 {
		t.Errorf("Content(cwd outside root) = %q files=%v", chain.Content, chain.Files)
	}

	// No AGENTS.md at all: empty chain.
	empty := t.TempDir()
	chain = loadAgentsChain(empty, empty)
	if chain.Content != "" || len(chain.Files) != 0 {
		t.Errorf("empty dir: Content=%q Files=%v, want empty", chain.Content, chain.Files)
	}
}

func TestLoadAgentsChainCap(t *testing.T) {
	root := t.TempDir()
	writeAgents(t, root, strings.Repeat("x", agentsChainCap+100))

	chain := loadAgentsChain(root, root)
	if !strings.HasSuffix(chain.Content, agentsTruncationMark) {
		t.Fatalf("capped chain should end with the truncation marker")
	}
	body := strings.TrimSuffix(chain.Content, agentsTruncationMark)
	if len(body) != agentsChainCap {
		t.Errorf("capped body = %d bytes, want %d", len(body), agentsChainCap)
	}
	if strings.Trim(body, "x") != "" {
		t.Errorf("capped body should be a prefix of the original content")
	}
}

func TestOverlayFreshness(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	rootFile := writeAgents(t, root, "v1")
	if err := os.Chtimes(rootFile, t0, t0); err != nil {
		t.Fatal(err)
	}

	// nil skill dirs: the test never scans the developer's real home skills.
	o := newOverlayDirs(root, sub, nil)
	if got := o.Content(); got != "v1" {
		t.Fatalf("initial content = %q, want v1", got)
	}

	// Unchanged mtimes: no rebuild, same composed string.
	if a, s := o.Refresh(); a || s {
		t.Error("refresh with unchanged mtime should report no change")
	}
	if got := o.Content(); got != "v1" {
		t.Errorf("content after no-op refresh = %q, want v1", got)
	}

	// mtime bump: rebuilt with the new content.
	if err := os.WriteFile(rootFile, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rootFile, t0.Add(2*time.Second), t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if a, _ := o.Refresh(); !a {
		t.Fatal("refresh after mtime change should report a change")
	}
	if got := o.Content(); got != "v2" {
		t.Errorf("content after reload = %q, want v2", got)
	}

	// File-set change (new AGENTS.md on the path): rebuilt.
	writeAgents(t, sub, "SUB")
	if a, _ := o.Refresh(); !a {
		t.Fatal("refresh after a new chain file should report a change")
	}
	if got, n := o.Content(), o.FileCount(); got != "v2\n\nSUB" || n != 2 {
		t.Errorf("content = %q fileCount = %d, want joined chain of 2 files", got, n)
	}

	// File-set change (removal): rebuilt back down to one file.
	if err := os.Remove(filepath.Join(sub, agentsFileName)); err != nil {
		t.Fatal(err)
	}
	if a, _ := o.Refresh(); !a {
		t.Fatal("refresh after a removed chain file should report a change")
	}
	if got, n := o.Content(), o.FileCount(); got != "v2" || n != 1 {
		t.Errorf("content = %q fileCount = %d after removal", got, n)
	}
}

func TestComposeSendHistory(t *testing.T) {
	history := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}

	// Empty overlay: the exact same slice, no copy (agent off = today's bytes).
	out := ComposeSendHistory(history, "")
	if len(out) != len(history) || &out[0] != &history[0] {
		t.Error("empty overlay should return the history slice itself")
	}

	// Overlay appends to the existing system message on a copy.
	out = ComposeSendHistory(history, "OVERLAY")
	if out[0].Content != "sys\n\nOVERLAY" {
		t.Errorf("send system = %q, want overlay appended", out[0].Content)
	}
	if out[1].Content != "hi" || len(out) != 2 {
		t.Errorf("send history tail changed: %+v", out)
	}
	if history[0].Content != "sys" {
		t.Errorf("history[0] mutated to %q — overlay leaked into clean history", history[0].Content)
	}

	// No user system prompt: a synthetic system message is inserted.
	noSys := []provider.Message{{Role: "user", Content: "hi"}}
	out = ComposeSendHistory(noSys, "OVERLAY")
	if len(out) != 2 || out[0].Role != "system" || out[0].Content != "OVERLAY" {
		t.Errorf("synthetic system message missing: %+v", out)
	}
	if len(noSys) != 1 || noSys[0].Role != "user" {
		t.Errorf("history mutated: %+v", noSys)
	}
}

// TestCleanHistoryAfterTurn simulates a full turn the way Run performs it: the
// send uses a composed copy while the user/assistant appends land in history —
// which must never carry the overlay text (it would otherwise be persisted).
func TestCleanHistoryAfterTurn(t *testing.T) {
	history := []provider.Message{{Role: "system", Content: "sys"}}
	history = append(history, provider.Message{Role: "user", Content: "question"})
	send := ComposeSendHistory(history, "OVERLAY")
	_ = send // the provider would receive this copy
	history = append(history, provider.Message{Role: "assistant", Content: "answer"})

	for i, m := range history {
		if strings.Contains(m.Content, "OVERLAY") {
			t.Errorf("history[%d] contains overlay text: %q", i, m.Content)
		}
	}
	if history[0].Content != "sys" {
		t.Errorf("history[0] = %q, want the user's own system prompt only", history[0].Content)
	}
}
