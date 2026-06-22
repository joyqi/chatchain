package chat

import (
	"io"
	"strings"
	"testing"

	"github.com/fatih/color"
)

// visible strips ANSI SGR codes, leaving the text the user actually sees.
func visible(s string) string { return ansiRe.ReplaceAllString(s, "") }

func TestHighlightInlineHidesMarkers(t *testing.T) {
	color.NoColor = false // force SGR codes even though `go test` isn't a TTY
	tests := []struct {
		in, want string
	}{
		{"**bold**", "bold"},
		{"__bold__", "bold"},
		{"*italic*", "italic"},
		{"_italic_", "italic"},
		{"`code`", "code"},
		{"a **b** and `c`", "a b and c"},
		{"see [docs](http://x) ok", "see docs (http://x) ok"},
		{"plain text", "plain text"},
	}
	for _, tt := range tests {
		got := visible(highlightInline(tt.in))
		if got != tt.want {
			t.Errorf("highlightInline(%q) visible = %q, want %q", tt.in, got, tt.want)
		}
	}
	// The styling must still be applied (markers hidden, not just deleted).
	if !strings.Contains(highlightInline("**x**"), "\x1b[") {
		t.Errorf("bold span lost its styling")
	}
}

func TestHighlightLineHidesMarkers(t *testing.T) {
	m := newMarkdownWriter(io.Discard)
	tests := []struct {
		in, want string
	}{
		{"# Title", "Title"},
		{"### Deep heading", "Deep heading"},
		{"> quoted", "▌ quoted"},
		{"- item", "• item"},
		{"* item", "• item"},
		{"1. first", "1. first"},
		{"  - nested", "  • nested"},
	}
	for _, tt := range tests {
		got := visible(m.highlightLine(tt.in))
		if got != tt.want {
			t.Errorf("highlightLine(%q) visible = %q, want %q", tt.in, got, tt.want)
		}
	}
}
