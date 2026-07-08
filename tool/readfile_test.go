package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// callReadFile runs the read_file tool with args and returns (text, isError).
func callReadFile(t *testing.T, args map[string]any) (string, bool) {
	t.Helper()
	rf, err := newReadFile(yaml.Node{})
	if err != nil {
		t.Fatal(err)
	}
	text, isErr, err := rf.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	return text, isErr
}

func TestReadFileWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Whole file: no window note.
	text, isErr := callReadFile(t, map[string]any{"path": path})
	if isErr || text != "l1\nl2\nl3\nl4\nl5" {
		t.Errorf("full read = (%q, %v), want the five lines", text, isErr)
	}

	// offset+limit window; integers arrive as float64 from JSON decoding.
	text, isErr = callReadFile(t, map[string]any{"path": path, "offset": float64(2), "limit": float64(2)})
	if isErr {
		t.Fatalf("windowed read errored: %q", text)
	}
	if !strings.HasPrefix(text, "l2\nl3") {
		t.Errorf("window = %q, want lines 2-3", text)
	}
	if !strings.Contains(text, "[showing lines 2-3 of 5]") {
		t.Errorf("window note missing: %q", text)
	}

	// offset alone: from there to EOF.
	text, isErr = callReadFile(t, map[string]any{"path": path, "offset": 4})
	if isErr || !strings.HasPrefix(text, "l4\nl5") {
		t.Errorf("offset-only read = (%q, %v), want lines 4-5", text, isErr)
	}

	// offset past EOF is a clear error.
	text, isErr = callReadFile(t, map[string]any{"path": path, "offset": 99})
	if !isErr || !strings.Contains(text, "past the end") {
		t.Errorf("offset past EOF = (%q, %v), want an error naming the overflow", text, isErr)
	}
}

func TestReadFileHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := callReadFile(t, map[string]any{"path": "~/note.txt"})
	if isErr || text != "hello" {
		t.Errorf("~ read = (%q, %v), want hello", text, isErr)
	}
}

func TestReadFileErrors(t *testing.T) {
	dir := t.TempDir()

	text, isErr := callReadFile(t, map[string]any{"path": filepath.Join(dir, "nope.txt")})
	if !isErr || !strings.Contains(text, "does not exist") {
		t.Errorf("missing file = (%q, %v), want a does-not-exist error", text, isErr)
	}

	text, isErr = callReadFile(t, map[string]any{"path": dir})
	if !isErr || !strings.Contains(text, "is a directory") {
		t.Errorf("directory = (%q, %v), want a directory error", text, isErr)
	}

	text, isErr = callReadFile(t, map[string]any{})
	if !isErr || !strings.Contains(text, "missing required argument") {
		t.Errorf("no path = (%q, %v), want a missing-argument error", text, isErr)
	}
}

func TestReadFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := callReadFile(t, map[string]any{"path": path})
	if isErr || text != "[file is empty]" {
		t.Errorf("empty file = (%q, %v), want the empty-file note", text, isErr)
	}
}

func TestReadFileTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	line := strings.Repeat("x", 99) + "\n" // 100 bytes per line
	if err := os.WriteFile(path, []byte(strings.Repeat(line, 1000)), 0o644); err != nil {
		t.Fatal(err) // ~100 KB, past the 64 KB output cap
	}

	text, isErr := callReadFile(t, map[string]any{"path": path})
	if isErr {
		t.Fatalf("big read errored: %q", text)
	}
	if len(text) > readFileMaxOutput+256 {
		t.Errorf("output = %d bytes, want it capped near %d", len(text), readFileMaxOutput)
	}
	if !strings.Contains(text, "truncated") || !strings.Contains(text, "offset=") || !strings.Contains(text, "limit") {
		t.Errorf("truncation marker should mention offset/limit continuation: %q", text[len(text)-300:])
	}
}

func TestRegistryEnable(t *testing.T) {
	// Agent mode: read_file auto-enabled without a config entry.
	reg := Build(nil, nil)
	reg.Enable("read_file", nil)
	defs := reg.Tools()
	if len(defs) != 1 || defs[0].Name != "read_file" {
		t.Fatalf("Tools() = %+v, want read_file alone", defs)
	}

	// Already enabled through config (empty node = defaults): no duplicate.
	reg = Build(map[string]yaml.Node{"read_file": {}}, nil)
	reg.Enable("read_file", nil)
	if got := len(reg.Tools()); got != 1 {
		t.Errorf("Tools() has %d entries, want 1 (Enable must not duplicate)", got)
	}

	// Unknown names warn and register nothing.
	var warned string
	reg = Build(nil, nil)
	reg.Enable("no_such_tool", func(format string, args ...any) { warned = fmt.Sprintf(format, args...) })
	if len(reg.Tools()) != 0 || !strings.Contains(warned, "no_such_tool") {
		t.Errorf("unknown tool: Tools()=%v warned=%q", reg.Tools(), warned)
	}
}
