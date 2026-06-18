package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadTools verifies the per-provider `tools:` block parses into raw nodes
// that each tool can decode itself (presence = enabled, empty = defaults).
func TestLoadTools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
providers:
  claude:
    type: anthropic
    key: sk-ant-xxx
    model: claude-sonnet-4
    tools:
      run_command:
        - git
        - ssh
  openai:
    key: sk-official
    tools:
      run_command:
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Load(path)

	// claude: run_command present with a populated list.
	_, pc := cfg.Get("claude")
	node, ok := pc.Tools["run_command"]
	if !ok {
		t.Fatal("claude: run_command should be present")
	}
	var allow []string
	if err := node.Decode(&allow); err != nil {
		t.Fatalf("decode allow: %v", err)
	}
	if want := []string{"git", "ssh"}; !reflect.DeepEqual(allow, want) {
		t.Fatalf("allow = %v, want %v", allow, want)
	}

	// openai: run_command present but empty (key exists → enabled, defaults).
	_, pc = cfg.Get("openai")
	if _, ok := pc.Tools["run_command"]; !ok {
		t.Fatal("openai: run_command key should be present even when empty")
	}

	// A provider without a tools block has no enabled tools.
	if _, pc := cfg.Get("missing"); len(pc.Tools) != 0 {
		t.Fatalf("missing provider should have no tools, got %v", pc.Tools)
	}
}
