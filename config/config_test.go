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
      shell:
        - git
        - ssh
  openai:
    key: sk-official
    tools:
      shell:
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Load(path)

	// claude: shell present with a populated list.
	_, pc := cfg.Get("claude")
	node, ok := pc.Tools["shell"]
	if !ok {
		t.Fatal("claude: shell should be present")
	}
	var allow []string
	if err := node.Decode(&allow); err != nil {
		t.Fatalf("decode allow: %v", err)
	}
	if want := []string{"git", "ssh"}; !reflect.DeepEqual(allow, want) {
		t.Fatalf("allow = %v, want %v", allow, want)
	}

	// openai: shell present but empty (key exists → enabled, defaults).
	_, pc = cfg.Get("openai")
	if _, ok := pc.Tools["shell"]; !ok {
		t.Fatal("openai: shell key should be present even when empty")
	}

	// A provider without a tools block has no enabled tools.
	if _, pc := cfg.Get("missing"); len(pc.Tools) != 0 {
		t.Fatalf("missing provider should have no tools, got %v", pc.Tools)
	}
}

// TestLoadAgent verifies the per-provider `agent:` switch accepts the YAML 1.1
// truthy spellings (true/yes/on — yaml.v3 resolves them all to bool) and
// defaults to off when absent.
func TestLoadAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
providers:
  a:
    agent: true
  b:
    agent: yes
  c:
    agent: on
  d:
    agent: false
  e:
    key: sk-xxx
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Load(path)
	for _, name := range []string{"a", "b", "c"} {
		if _, pc := cfg.Get(name); !pc.Agent {
			t.Errorf("provider %s: agent should be enabled", name)
		}
	}
	for _, name := range []string{"d", "e", "missing"} {
		if _, pc := cfg.Get(name); pc.Agent {
			t.Errorf("provider %s: agent should be disabled", name)
		}
	}
}

// system_file: inline system wins; otherwise the file's contents are the
// prompt; a configured but unreadable file is a hard error.
func TestResolveSystem(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sys.md")
	os.WriteFile(f, []byte("You are terse.\n"), 0o644)

	if got, err := (ProviderConfig{System: "inline", SystemFile: f}).ResolveSystem(); err != nil || got != "inline" {
		t.Fatalf("inline should win: %q, %v", got, err)
	}
	if got, err := (ProviderConfig{SystemFile: f}).ResolveSystem(); err != nil || got != "You are terse.\n" {
		t.Fatalf("file read: %q, %v", got, err)
	}
	if _, err := (ProviderConfig{SystemFile: filepath.Join(dir, "missing.md")}).ResolveSystem(); err == nil {
		t.Fatal("missing system_file must error, not silently blank the prompt")
	}
	if got, err := (ProviderConfig{}).ResolveSystem(); err != nil || got != "" {
		t.Fatalf("neither set: %q, %v", got, err)
	}
}

// Provider key/url/system_file expand ${…} variables at load time.
func TestLoadExpandsProviderVars(t *testing.T) {
	t.Setenv("CFG_TEST_KEY", "sk-expanded")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	os.WriteFile(cfgPath, []byte(
		"providers:\n  d:\n    type: openai\n    key: ${env:CFG_TEST_KEY}\n    url: ${env:CFG_TEST_KEY}/v1\n    system_file: ${appHome}/sys.md\n    effort: high\n"), 0o644)

	cfg := Load(cfgPath)
	_, pc := cfg.Get("d")
	if pc.Key != "sk-expanded" {
		t.Errorf("key = %q", pc.Key)
	}
	if pc.URL != "sk-expanded/v1" {
		t.Errorf("url = %q", pc.URL)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".chatchain", "sys.md"); pc.SystemFile != want {
		t.Errorf("system_file = %q, want %q", pc.SystemFile, want)
	}
	if pc.Effort != "high" {
		t.Errorf("effort = %q", pc.Effort)
	}
}

// The provider mcp_servers key: absent = all servers, empty list = none,
// names = exactly that subset, unknown name = loud error. The nil-vs-empty
// distinction must survive YAML decoding.
func TestMCPServersFor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	os.WriteFile(cfgPath, []byte(`
providers:
  all:
    type: openai
  none:
    type: openai
    mcp_servers: []
  some:
    type: openai
    mcp_servers: [fs]
  typo:
    type: openai
    mcp_servers: [nope]
mcp_servers:
  fs:
    command: fs-server
  gh:
    url: https://x/sse
`), 0o644)
	cfg := Load(cfgPath)

	_, pc := cfg.Get("all")
	if got, err := cfg.MCPServersFor(pc); err != nil || len(got) != 2 {
		t.Fatalf("absent key: %d servers, %v (want all 2)", len(got), err)
	}
	_, pc = cfg.Get("none")
	if got, err := cfg.MCPServersFor(pc); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %d servers, %v (want 0)", len(got), err)
	}
	_, pc = cfg.Get("some")
	got, err := cfg.MCPServersFor(pc)
	if err != nil || len(got) != 1 {
		t.Fatalf("subset: %d servers, %v (want 1)", len(got), err)
	}
	if got["fs"].Command != "fs-server" {
		t.Fatalf("subset picked the wrong server: %+v", got)
	}
	_, pc = cfg.Get("typo")
	if _, err := cfg.MCPServersFor(pc); err == nil {
		t.Fatal("unknown name must error, not silently skip")
	}
}

// notify parses as a switch and defaults to nil — absent means ON, so only
// an explicit false silences the attention channels.
func TestNotifyField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	os.WriteFile(cfgPath, []byte("providers:\n  quiet:\n    type: openai\n    notify: false\n  norm:\n    type: openai\n"), 0o644)
	cfg := Load(cfgPath)
	if _, pc := cfg.Get("quiet"); pc.Notify == nil || *pc.Notify {
		t.Errorf("notify: false not parsed (got %v)", pc.Notify)
	}
	if _, pc := cfg.Get("norm"); pc.Notify != nil {
		t.Errorf("notify must default nil (on), got %v", *pc.Notify)
	}
}

// no_save parses and defaults false.
func TestNoSaveField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	os.WriteFile(cfgPath, []byte("providers:\n  eph:\n    type: openai\n    no_save: true\n  norm:\n    type: openai\n"), 0o644)
	cfg := Load(cfgPath)
	if _, pc := cfg.Get("eph"); !pc.NoSave {
		t.Error("no_save: true not parsed")
	}
	if _, pc := cfg.Get("norm"); pc.NoSave {
		t.Error("no_save must default false")
	}
}
