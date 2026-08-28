// Package app pins the program's identity — the one place its name is
// spelled — and resolves the per-user directory that hangs off it. Every
// user-visible spelling (the binary, the terminal-title fallback, host status
// rows, export file prefixes, ~/.iota) derives from here, so a rename is a
// one-constant change plus a MigrateLegacy entry.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Name is the command and brand name — lower-case everywhere, like the
// domain (iota.sh) and the binary.
const Name = "iota"

// ConfigBase is the config file stem: ~/.iota.yaml, ./.iota.yml, ….
const ConfigBase = "." + Name

// dotDir is the per-user directory under $HOME (sessions, generated images,
// user skills — everything ${appHome} points at).
const dotDir = "." + Name

// The program shipped as "chatchain" through v2.16. The pre-rename names are
// still recognised so an upgrade is invisible: the global ones move once
// (MigrateLegacy); a project-local config file is read in place (it lives in
// somebody's working tree and is never touched).
const (
	LegacyName       = "chatchain"
	LegacyConfigBase = "." + LegacyName
	legacyDotDir     = "." + LegacyName
)

// ConfigExts lists the config file extensions in lookup order.
var ConfigExts = []string{".yaml", ".yml"}

// Home returns the per-user directory (~/.iota).
func Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dotDir), nil
}

// MigrateLegacy moves the pre-rename global files into place — ~/.chatchain
// to ~/.iota and ~/.chatchain.y(a)ml to ~/.iota.y(a)ml — under home (the
// user's home directory) and returns one notice per move for the caller to
// print. It is idempotent: nothing happens once the legacy names are gone,
// and a current name that already exists always wins — never overwritten,
// the legacy item is left where it is. A failed rename is reported, not
// fatal: the legacy config still loads through the lookup fallback, and the
// directory simply waits to be moved by hand.
func MigrateLegacy(home string) []string {
	var notes []string
	move := func(old, cur string) {
		if _, err := os.Lstat(old); err != nil {
			return // nothing to migrate
		}
		if _, err := os.Lstat(cur); err == nil {
			return // current name already in use: leave both alone
		}
		if err := os.Rename(old, cur); err != nil {
			notes = append(notes, fmt.Sprintf("%s: could not move %s to %s: %v", Name, tilde(home, old), tilde(home, cur), err))
			return
		}
		notes = append(notes, fmt.Sprintf("%s: moved %s → %s", Name, tilde(home, old), tilde(home, cur)))
	}
	move(filepath.Join(home, legacyDotDir), filepath.Join(home, dotDir))
	// Config: skipped entirely while any current-name file exists, so the two
	// extensions can never end up half-migrated with the wrong one winning.
	for _, ext := range ConfigExts {
		if _, err := os.Lstat(filepath.Join(home, ConfigBase+ext)); err == nil {
			return notes
		}
	}
	for _, ext := range ConfigExts {
		move(filepath.Join(home, LegacyConfigBase+ext), filepath.Join(home, ConfigBase+ext))
	}
	return notes
}

// tilde renders a path under home as ~/…, the way users write it.
func tilde(home, p string) string {
	if rel, err := filepath.Rel(home, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}
