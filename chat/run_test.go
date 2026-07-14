package chat

import "testing"

// Ported v1 contracts (chat/title_test.go, chat/inputchrome_test.go — died
// with the old stack): the tab title falls back to the app name for empty and
// whitespace-only session titles, and the status line's model field falls
// back to the provider type until a model is chosen.
func TestWindowTitleFallback(t *testing.T) {
	tests := []struct{ in, want string }{
		{"My chat", "My chat"},
		{"", appTitle},
		{"   \t ", appTitle},
	}
	for _, tt := range tests {
		if got := windowTitle(tt.in); got != tt.want {
			t.Errorf("windowTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStatusModelLabelFallsBackToType(t *testing.T) {
	if got := statusModelLabel("gpt-4o", "openai"); got != "gpt-4o" {
		t.Errorf("model label = %q, want gpt-4o", got)
	}
	if got := statusModelLabel("", "openai"); got != "openai" {
		t.Errorf("empty model label = %q, want provider type", got)
	}
}

// TestContextBudgetStatus pins the status string the /compact flow prints:
// "used / window (pct)" with a leading ≈ while usage is a local estimate
// (ported from the v1 composer-status tests).
func TestContextBudgetStatus(t *testing.T) {
	b := newContextBudget(128000)
	if got := b.status(); got != "≈0 / 128k (0%)" {
		t.Errorf("fresh status = %q, want ≈0 / 128k (0%%)", got)
	}
	b.used = 64000
	b.haveUsage = true
	if got := b.status(); got != "64k / 128k (50%)" {
		t.Errorf("exact status = %q, want 64k / 128k (50%%)", got)
	}
}
