package chat

import (
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
)

// firstUserText is the sole naming input: the first user message with any
// text. An attachment-only opener defers to the next message, and the
// assistant reply is never consulted — the name must not wait for it.
func TestFirstUserText(t *testing.T) {
	if got := firstUserText(nil); got != "" {
		t.Fatalf("empty history: %q", got)
	}
	if got := firstUserText([]provider.Message{{Role: "user", Content: "draw a cat"}}); got != "draw a cat" {
		t.Fatalf("single message: %q", got)
	}
	if got := firstUserText([]provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Attachments: []provider.Attachment{{Filename: "a.png"}}},
		{Role: "assistant", Content: "what a nice picture"},
		{Role: "user", Content: "hi"},
	}); got != "hi" {
		t.Fatalf("attachment-only opener must defer to the next text: %q", got)
	}
}

// Every title entry point funnels through titleFrom: one line, no control
// characters, capped on rune boundaries. A stored newline would break the
// session picker's row accounting — image providers hit this constantly,
// since their title IS the prompt (no model summarizes it).
func TestTitleFrom(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"multi-line prompt": {"draw a cat\nwith a hat\nand boots", "draw a cat with a hat and boots"},
		"tabs and runs":     {"draw \t a\n\n  cat", "draw a cat"},
		"control chars":     {"draw\x00 a\x07 cat", "draw a cat"},
		"already one line":  {"draw a cat", "draw a cat"},
		"blank":             {"   \n\t ", ""},
	} {
		if got := titleFrom(tc.in, 40); got != tc.want {
			t.Errorf("%s: titleFrom(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}

	// The cap counts runes, so CJK is never cut mid-character.
	long := strings.Repeat("生成一张图片", 10) // 60 runes
	got := titleFrom(long, 40)
	if r := []rune(got); len(r) != 41 || r[40] != '…' {
		t.Fatalf("cap = %d runes (%q)", len(r), got)
	}
}

// The model's own answer keeps its first-line semantics (an explanatory
// second paragraph is not part of the title) and still lands flattened.
func TestSanitizeTitle(t *testing.T) {
	if got := sanitizeTitle("  \"Cat portrait\"  \n\nI chose this because…"); got != "Cat portrait" {
		t.Fatalf("sanitizeTitle = %q", got)
	}
	if got := sanitizeTitle("Cat\tportrait"); got != "Cat portrait" {
		t.Fatalf("tab survived: %q", got)
	}
}

// Reasoning models behind chatcomp relays leak <think> blocks into plain
// content; a chain of thought must never become the session title.
func TestSanitizeTitleStripsThink(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"block before title": {"<think>\nthe user wants…\n</think>\nCat portrait", "Cat portrait"},
		"unclosed tag":       {"<think>never closed, all thought", ""},
		"multiple blocks":    {"A<think>x</think>B<think>y</think>C", "ABC"},
		"no block":           {"Cat portrait", "Cat portrait"},
	} {
		if got := sanitizeTitle(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeTitle(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
