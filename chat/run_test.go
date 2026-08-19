package chat

import (
	"strings"
	"testing"
)

// Ported v1 contracts (chat/title_test.go, chat/inputchrome_test.go — died
// with the old stack): the tab title falls back to the app name for empty and
// whitespace-only session titles, and the status line's model field falls
// back to the provider type until a model is chosen.

// The completion notification carries a digest of the answer: first content
// line, markdown stripped, capped — never a heading marker or a blank.
func TestNotifyDigest(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"heading stripped": {"## The fix\n\ndetails follow", "The fix"},
		"list and bold":    {"- **Done**: `run.go` updated", "Done: run.go updated"},
		"leading blanks":   {"\n\n\nplain answer", "plain answer"},
		"empty reply":      {"", "Response ready"},
		"whitespace only":  {"  \n\t\n", "Response ready"},
		"quote block":      {"> quoted insight", "quoted insight"},
	} {
		if got := notifyDigest(tc.in); got != tc.want {
			t.Errorf("%s: notifyDigest(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}

	// CJK caps on rune boundaries with an ellipsis.
	long := strings.Repeat("很长的回复", 20)
	got := notifyDigest(long)
	if r := []rune(got); len(r) != 61 || r[60] != '…' {
		t.Errorf("long digest = %q (%d runes)", got, len(r))
	}
}

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
	b.settled = 64000
	b.haveUsage = true
	if got := b.status(); got != "64k / 128k (50%)" {
		t.Errorf("exact status = %q, want 64k / 128k (50%%)", got)
	}
}
