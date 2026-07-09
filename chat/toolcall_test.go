package chat

import (
	"bytes"
	"strings"
	"testing"

	"chatchain/provider"

	"github.com/fatih/color"
)

func TestToolCallHeader(t *testing.T) {
	tests := []struct {
		name string
		tc   provider.ToolCall
		want string
	}{
		{"single arg", provider.ToolCall{Name: "run_command", Arguments: map[string]any{"command": "git status"}}, "[run_command command:git status]"},
		{"keys sorted", provider.ToolCall{Name: "run_command", Arguments: map[string]any{"command": "git", "cwd": "/tmp", "stdin": "hi"}}, "[run_command command:git cwd:/tmp stdin:hi]"},
		{"no args", provider.ToolCall{Name: "ping"}, "[ping]"},
		{"newline collapsed", provider.ToolCall{Name: "x", Arguments: map[string]any{"a": "l1\nl2"}}, "[x a:l1 l2]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolCallHeader(tt.tc); got != tt.want {
				t.Errorf("toolCallHeader() = %q, want %q", got, tt.want)
			}
		})
	}

	// Long values are truncated to toolHeaderMaxValue runes + ellipsis.
	got := toolCallHeader(provider.ToolCall{Name: "x", Arguments: map[string]any{"a": strings.Repeat("z", 100)}})
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
