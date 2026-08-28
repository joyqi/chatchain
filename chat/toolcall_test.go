package chat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
	"github.com/joyqi/iota/tool"

	"github.com/fatih/color"
)

func TestToolCallHeader(t *testing.T) {
	tests := []struct {
		name string
		tc   provider.ToolCall
		want string
	}{
		{"single arg", provider.ToolCall{Name: "bash", Arguments: map[string]any{"command": "git status"}}, "[bash command:git status]"},
		{"keys sorted", provider.ToolCall{Name: "bash", Arguments: map[string]any{"command": "git", "cwd": "/tmp", "stdin": "hi"}}, "[bash command:git cwd:/tmp stdin:hi]"},
		{"no args", provider.ToolCall{Name: "ping"}, "[ping]"},
		{"newline collapsed", provider.ToolCall{Name: "x", Arguments: map[string]any{"a": "l1\nl2"}}, "[x a:l1 l2]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolCallHeader(nil, tt.tc); got != tt.want {
				t.Errorf("toolCallHeader() = %q, want %q", got, tt.want)
			}
		})
	}

	// Long values are truncated to toolHeaderMaxValue runes + ellipsis.
	got := toolCallHeader(nil, provider.ToolCall{Name: "x", Arguments: map[string]any{"a": strings.Repeat("z", 100)}})
	if !strings.Contains(got, strings.Repeat("z", toolHeaderMaxValue)+"…") || strings.Contains(got, strings.Repeat("z", toolHeaderMaxValue+1)) {
		t.Errorf("long value not truncated to %d runes + ellipsis: %q", toolHeaderMaxValue, got)
	}
}

func TestPrintToolResult(t *testing.T) {
	old := color.NoColor
	color.NoColor = true // deterministic output, no ANSI
	defer func() { color.NoColor = old }()

	render := func(result string, isErr bool) string {
		var b bytes.Buffer
		printToolResult(&b, result, isErr)
		return b.String()
	}

	tests := []struct {
		name, result, want string
	}{
		{"two lines shown fully", "line1\nline2", "  ⎿ line1\n    line2\n"},
		{"exactly three shown fully", "a\nb\nc", "  ⎿ a\n    b\n    c\n"},
		{"over three truncates with tail", "a\nb\nc\nd\ne", "  ⎿ a\n    b\n    … +3 lines\n"},
		{"trailing blank lines trimmed", "only\n\n\n", "  ⎿ only\n"},
		{"empty becomes no output", "   \n  ", "  ⎿ (no output)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := render(tt.result, false); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// headerDispatch is a Dispatcher carrying the HeaderReporter capability.
type headerDispatch struct {
	tool.Dispatcher
	summary string
	ok      bool
}

func (d headerDispatch) HeaderSummary(string, map[string]any) (string, bool) {
	return d.summary, d.ok
}

// A tool that writes its own summary takes over the header completely: an
// empty one is a bare name, NOT a fallback to the argument digest (which for
// edit_file would paste a whole file into the header).
func TestToolCallHeaderCapability(t *testing.T) {
	tc := provider.ToolCall{Name: "edit_file", Arguments: map[string]any{
		"path":       "internal/ui/model.go",
		"new_string": strings.Repeat("code\n", 500),
	}}

	if got := toolCallHeader(headerDispatch{summary: "internal/ui/model.go", ok: true}, tc); got != "[edit_file internal/ui/model.go]" {
		t.Fatalf("custom summary = %q", got)
	}
	if got := toolCallHeader(headerDispatch{summary: "", ok: true}, tc); got != "[edit_file]" {
		t.Fatalf("empty summary = %q, want a bare name", got)
	}

	// No capability declared: the generic digest applies, unchanged.
	fallback := toolCallHeader(headerDispatch{ok: false}, tc)
	if !strings.Contains(fallback, "path:") {
		t.Fatalf("digest not used when the tool declares no summary: %q", fallback)
	}
}
