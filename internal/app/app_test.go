package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != ".iota" {
		t.Errorf("Home() = %q, want …/.iota", got)
	}
}

// An upgrade from the chatchain days moves the global directory and config
// file once, announces each move, and is silent from then on.
func TestMigrateLegacyMoves(t *testing.T) {
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".chatchain", "sessions", "abc"))
	mustWrite(t, filepath.Join(home, ".chatchain.yml"), "providers: {}\n")

	notes := MigrateLegacy(home)
	if len(notes) != 2 {
		t.Fatalf("notes = %q, want two moves", notes)
	}
	for _, n := range notes {
		if !strings.HasPrefix(n, "iota: moved ~/.chatchain") {
			t.Errorf("note %q: want an iota: moved ~/… line", n)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".iota", "sessions", "abc")); err != nil {
		t.Errorf("sessions not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".iota.yml")); err != nil {
		t.Errorf("config not moved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".chatchain")); !os.IsNotExist(err) {
		t.Error("legacy directory still present")
	}
	if again := MigrateLegacy(home); len(again) != 0 {
		t.Errorf("second run = %q, want nothing", again)
	}
}

// A current-name item that already exists is never overwritten: the legacy
// one stays put and nothing is announced.
func TestMigrateLegacyNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".chatchain"))
	mustMkdirAll(t, filepath.Join(home, ".iota"))
	mustWrite(t, filepath.Join(home, ".chatchain.yaml"), "legacy: true\n")
	mustWrite(t, filepath.Join(home, ".iota.yml"), "current: true\n")

	if notes := MigrateLegacy(home); len(notes) != 0 {
		t.Errorf("notes = %q, want none", notes)
	}
	if _, err := os.Stat(filepath.Join(home, ".chatchain")); err != nil {
		t.Error("legacy directory should be left alone")
	}
	if _, err := os.Stat(filepath.Join(home, ".chatchain.yaml")); err != nil {
		t.Error("legacy config should be left alone")
	}
	if _, err := os.Stat(filepath.Join(home, ".iota.yaml")); !os.IsNotExist(err) {
		t.Error("a .iota.yaml must not appear beside the existing .iota.yml")
	}
}

func TestMigrateLegacyNothingToDo(t *testing.T) {
	if notes := MigrateLegacy(t.TempDir()); len(notes) != 0 {
		t.Errorf("notes = %q, want none", notes)
	}
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
