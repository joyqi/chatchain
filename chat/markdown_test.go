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

func TestTableRender(t *testing.T) {
	color.NoColor = false
	src := "| Name | Note |\n|------|------|\n| `--key` | the **secret** |\n| x | y |\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := visible(out.String())

	// Box-drawing borders are present.
	if !strings.Contains(got, "┌") || !strings.Contains(got, "│") {
		t.Errorf("table missing box-drawing:\n%s", got)
	}
	// Inline markers are hidden inside cells.
	for _, bad := range []string{"`--key`", "**secret**", "|---"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered table still shows markup %q:\n%s", bad, got)
		}
	}
	// Cell contents survive (markers stripped).
	for _, want := range []string{"--key", "secret", "Name", "Note"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered table missing %q:\n%s", want, got)
		}
	}
}

func TestCodeBlockRender(t *testing.T) {
	color.NoColor = false
	src := "```python\ndef f():\n    return 1\n```\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := out.String()

	if strings.Contains(got, "```") {
		t.Errorf("code fence not hidden:\n%q", got)
	}
	v := strings.TrimRight(visible(got), "\n")
	for _, ln := range strings.Split(v, "\n") {
		if !strings.HasPrefix(ln, "  ") {
			t.Errorf("code line not indented by two spaces: %q", ln)
		}
	}
	if !strings.Contains(v, "def f():") || !strings.Contains(v, "return 1") {
		t.Errorf("code content missing:\n%q", v)
	}
	if !strings.Contains(got, "\x1b[38;5;") {
		t.Errorf("code not syntax-highlighted:\n%q", got)
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
