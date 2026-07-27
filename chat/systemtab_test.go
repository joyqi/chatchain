package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatchain/config"
	"chatchain/internal/ui"
	"chatchain/provider"
)

// TestSystemPromptPanelAbsent: no system message (or an empty one) means no
// tab — the /model surface stays as it was rather than showing an empty view.
func TestSystemPromptPanelAbsent(t *testing.T) {
	cases := []struct {
		name    string
		history []provider.Message
	}{
		{"no history", nil},
		{"conversation only", []provider.Message{{Role: "user", Content: "hi"}}},
		{"empty system", []provider.Message{{Role: "system", Content: ""}, {Role: "user", Content: "hi"}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := systemPromptPanel(tt.history, ""); ok {
				t.Error("panel offered with no system prompt in effect")
			}
		})
	}
}

// TestSystemPromptPanelShape checks the read-only tab: a View panel that wraps,
// carrying the prompt split into lines.
func TestSystemPromptPanelShape(t *testing.T) {
	history := []provider.Message{
		{Role: "system", Content: "You are terse.\nAnswer in one line."},
		{Role: "user", Content: "hi"},
	}
	p, ok := systemPromptPanel(history, "")
	if !ok {
		t.Fatal("panel not offered for a chat with a system prompt")
	}
	if p.Title != "System" || p.Kind != ui.PanelView || !p.Wrap {
		t.Errorf("panel shape = %q/%v/wrap=%v, want System/PanelView/wrap=true", p.Title, p.Kind, p.Wrap)
	}
	want := []string{"You are terse.", "Answer in one line."}
	if len(p.Lines) != len(want) {
		t.Fatalf("Lines = %q, want %q", p.Lines, want)
	}
	for i := range want {
		if p.Lines[i] != want[i] {
			t.Errorf("Lines[%d] = %q, want %q", i, p.Lines[i], want[i])
		}
	}
}

// TestSystemPromptPanelFromConfig pins the config path end to end: the
// `system_file:` key resolves to the same history[0] the tab reads, so a
// prompt defined in the config file shows up like a -s one.
func TestSystemPromptPanelFromConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sys.md")
	if err := os.WriteFile(file, []byte("You are a config-defined assistant."), 0o600); err != nil {
		t.Fatal(err)
	}
	sys, err := config.ProviderConfig{SystemFile: file}.ResolveSystem()
	if err != nil {
		t.Fatal(err)
	}
	// cmd/root.go hands the resolved text to chat.Run, which seeds history[0].
	history := []provider.Message{{Role: "system", Content: sys}}
	p, ok := systemPromptPanel(history, "")
	if !ok {
		t.Fatal("config-defined system prompt produced no tab")
	}
	if got := strings.Join(p.Lines, "\n"); got != "You are a config-defined assistant." {
		t.Errorf("Lines = %q, want the config file's text", got)
	}
}

// TestSystemPromptPanelIncludesOverlay: agent mode appends the AGENTS.md
// overlay to the system message at send time, so the tab — showing the prompt
// AS SENT — must carry both parts.
func TestSystemPromptPanelIncludesOverlay(t *testing.T) {
	history := []provider.Message{{Role: "system", Content: "Base prompt."}}
	p, ok := systemPromptPanel(history, "# AGENTS.md\nProject rules.")
	if !ok {
		t.Fatal("panel not offered")
	}
	body := strings.Join(p.Lines, "\n")
	if !strings.Contains(body, "Base prompt.") || !strings.Contains(body, "Project rules.") {
		t.Errorf("Lines = %q, want both the base prompt and the overlay", body)
	}
}

// TestSystemPromptPanelOverlayOnly covers agent mode without any user prompt:
// ComposeSendHistory synthesizes the system message, and the tab shows it.
func TestSystemPromptPanelOverlayOnly(t *testing.T) {
	p, ok := systemPromptPanel([]provider.Message{{Role: "user", Content: "hi"}}, "Project rules.")
	if !ok {
		t.Fatal("overlay-only agent mode produced no tab")
	}
	if got := strings.Join(p.Lines, "\n"); got != "Project rules." {
		t.Errorf("Lines = %q, want the overlay text", got)
	}
}
