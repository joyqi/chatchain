package ui

import (
	"strings"
	"testing"
)

// The renderer emits View.WindowTitle as a raw OSC sequence with no escaping
// (ansi.SetWindowTitle), so SetTitle sanitizes: control bytes are stripped —
// a crafted session title can never terminate or escape the sequence — and
// the length is bounded to a sensible tab label. Ported from the v1
// terminalTitleSeq contract (chat/title_test.go, died with the old stack).
func TestSanitizeWindowTitle(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain CJK", "二次方程求根公式", "二次方程求根公式"},
		{"ascii", "My chat", "My chat"},
		{"trimmed", "  hello  ", "hello"},
		{"injection stripped", "a\x1b]0;evil\x07b\nc", "a]0;evilbc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeWindowTitle(tt.in); got != tt.want {
				t.Errorf("sanitizeWindowTitle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeWindowTitleTruncates(t *testing.T) {
	got := sanitizeWindowTitle(strings.Repeat("字", 100))
	if r := []rune(got); len(r) != 61 || r[60] != '…' { // 60 runes + ellipsis
		t.Errorf("title not truncated to 60 runes + ellipsis: %d runes %q", len([]rune(got)), got)
	}
}
