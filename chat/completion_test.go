package chat

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fatih/color"
)

func TestSlashHelpers(t *testing.T) {
	for _, tok := range []string{"/file", "/session", "/tools"} {
		if !isSlashCommand(tok) {
			t.Errorf("isSlashCommand(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"/fil", "/", "/file ", "/nope", "file"} {
		if isSlashCommand(tok) {
			t.Errorf("isSlashCommand(%q) = true, want false", tok)
		}
	}
	for _, tok := range []string{"/", "/f", "/fi", "/file", "/m", "/context"} {
		if !isSlashPrefix(tok) {
			t.Errorf("isSlashPrefix(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"/x", "/zz", "/MODEL", "x/"} {
		if isSlashPrefix(tok) {
			t.Errorf("isSlashPrefix(%q) = true, want false", tok)
		}
	}
}

// reuses ansiRe from markdown.go
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// commandPainter must never change the visible text (only add zero-width color),
// or readline's cursor math — which measures the logical buffer — would desync.
func TestCommandPainter(t *testing.T) {
	color.NoColor = false // force escapes so the coloring path is exercised
	defer func() { color.NoColor = true }()

	cases := []struct {
		in      string
		colored bool
	}{
		{"", false},
		{"hello world", false},
		{"/xyz", false},         // neither a command nor a prefix
		{"/MODEL", false},       // case-sensitive: not a known command
		{"/fi", true},           // valid prefix → cyan
		{"/file", true},         // exact command → green
		{"/file foo bar", true}, // command + args; only the token is painted
		{"/context 1m", true},   //
	}
	for _, c := range cases {
		out := string(commandPainter([]rune(c.in), len([]rune(c.in))))
		if got := stripANSI(out); got != c.in {
			t.Errorf("painter(%q): stripped=%q, want %q (visible text must be preserved)", c.in, got, c.in)
		}
		if colored := out != c.in; colored != c.colored {
			t.Errorf("painter(%q): colored=%v, want %v", c.in, colored, c.colored)
		}
	}
}

// slashTriggerReader injects a Tab after "/" only when the line is empty (the
// flag the Listener maintains), and resets that flag on Enter / Ctrl+C.
func TestSlashTriggerReader(t *testing.T) {
	const tab = "\t"
	cases := []struct {
		in    string
		empty bool // initial line-empty flag
		want  string
	}{
		{"/", true, "/" + tab},              // "/" on an empty line → inject Tab
		{"/", false, "/"},                   // "/" on a non-empty line → no trigger
		{"/file", true, "/" + tab + "file"}, // only the leading "/" triggers
		{"//", true, "/" + tab + "/"},       // first "/" triggers; line now non-empty
		{"hi", true, "hi"},                  // ordinary chars pass through
		{"a\r/", false, "a\r/" + tab},       // Enter resets the flag → next "/" triggers
		{"x\x03/", false, "x\x03/" + tab},   // Ctrl+C resets the flag
		{"你好", true, "你好"},                  // CJK passes through untouched (no 0x2f)
	}
	for _, c := range cases {
		var empty atomic.Bool
		empty.Store(c.empty)
		r := newSlashTriggerReader(strings.NewReader(c.in), &empty)
		buf := make([]byte, 4) // small buffer to exercise the overflow queue
		var got string
		for {
			n, err := r.Read(buf)
			got += string(buf[:n])
			if err != nil || (n == 0 && len(r.queue) == 0) {
				break
			}
		}
		if got != c.want {
			t.Errorf("Read(%q, empty=%v) = %q, want %q", c.in, c.empty, got, c.want)
		}
	}
}

// slashTriggerListener tracks readline's real buffer emptiness, forces a full
// repaint on command lines, and composes the base listener.
func TestSlashTriggerListener(t *testing.T) {
	var empty atomic.Bool
	l := slashTriggerListener(&empty, nil)

	// Command line: lineEmpty=false and ok=true (force full repaint).
	if nl, np, ok := l([]rune("/file"), 5, 'e'); !ok || string(nl) != "/file" || np != 5 {
		t.Errorf("command line: got (%q,%d,%v), want (\"/file\",5,true)", string(nl), np, ok)
	}
	if empty.Load() {
		t.Error("non-empty buffer should set lineEmpty=false")
	}

	// Empty line: lineEmpty=true, no forced repaint.
	if _, _, ok := l(nil, 0, 0); ok {
		t.Error("empty line should not force a repaint")
	}
	if !empty.Load() {
		t.Error("empty buffer should set lineEmpty=true")
	}

	// Ordinary (non-command) line: keep the fast path (ok=false).
	if _, _, ok := l([]rune("hello"), 5, 'o'); ok {
		t.Error("ordinary line should not force a repaint")
	}

	// A base listener that rewrites the line takes priority.
	base := func(line []rune, pos int, key rune) ([]rune, int, bool) {
		return []rune("X"), 1, true
	}
	if nl, np, ok := slashTriggerListener(&empty, base)([]rune("/file"), 5, 'e'); !ok || string(nl) != "X" || np != 1 {
		t.Errorf("base rewrite should win: got (%q,%d,%v), want (\"X\",1,true)", string(nl), np, ok)
	}
}
