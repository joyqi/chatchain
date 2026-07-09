package chat

import (
	"strings"
	"testing"
)

// TestTerminalTitleSeq checks the OSC 0 sequence that mirrors the session title
// into the terminal window/tab title: a plain title is wrapped in ESC ] 0 ; … BEL,
// an empty title falls back to the app name, and control bytes are stripped so a
// crafted title can never terminate or escape the sequence.
func TestTerminalTitleSeq(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "二次方程求根公式", "\033]0;二次方程求根公式\a"},
		{"ascii", "My chat", "\033]0;My chat\a"},
		{"empty falls back", "", "\033]0;" + appTitle + "\a"},
		{"whitespace only falls back", "   \t ", "\033]0;" + appTitle + "\a"},
		{"trimmed", "  hello  ", "\033]0;hello\a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalTitleSeq(tt.in); got != tt.want {
				t.Errorf("terminalTitleSeq(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A title carrying its own ESC/BEL bytes (an injection attempt) has them stripped
// so the emitted sequence has exactly one opening OSC and one BEL terminator —
// the injected control chars cannot break out into a second command.
func TestTerminalTitleSeqStripsControlBytes(t *testing.T) {
	got := terminalTitleSeq("a\x1b]0;evil\x07b\nc")
	if strings.Count(got, "\a") != 1 {
		t.Errorf("expected exactly one BEL terminator, got %q", got)
	}
	if strings.Count(got, "\x1b") != 1 { // only the leading ESC of our own OSC
		t.Errorf("injected ESC not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "\033]0;") || !strings.HasSuffix(got, "\a") {
		t.Errorf("malformed sequence: %q", got)
	}
	// The visible title keeps the harmless characters, dropped only the controls.
	if !strings.Contains(got, "a]0;evilbc") {
		t.Errorf("title body mangled: %q", got)
	}
}

// A long title is bounded (rune-safe) so it stays a sensible tab label.
func TestTerminalTitleSeqTruncates(t *testing.T) {
	seq := terminalTitleSeq(strings.Repeat("字", 100))
	body := strings.TrimSuffix(strings.TrimPrefix(seq, "\033]0;"), "\a")
	if r := []rune(body); len(r) != 61 || r[60] != '…' { // 60 runes + ellipsis
		t.Errorf("title not truncated to 60 runes + ellipsis: %d runes", len([]rune(body)))
	}
}
