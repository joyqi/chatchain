package chat

import (
	"strings"
	"testing"

	"chatchain/provider"
)

// titleSeeds drives maybeTitle: image-only assistant replies (dedicated image
// providers) must yield a prompt-derived title with NO LLM pass — asking an
// image provider for a title would paint a picture.
func TestTitleSeeds(t *testing.T) {
	img := provider.Attachment{Filename: "image-1.png", MimeType: "image/png", Data: []byte{1}}

	u, a, imageReply := titleSeeds([]provider.Message{{Role: "user", Content: "draw a cat"}})
	if u != "draw a cat" || a != "" || imageReply {
		t.Fatalf("no assistant yet: %q %q %v", u, a, imageReply)
	}

	u, a, imageReply = titleSeeds([]provider.Message{
		{Role: "user", Content: "draw a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{img}},
	})
	if u != "draw a cat" || a != "" || !imageReply {
		t.Fatalf("image-only reply: %q %q %v", u, a, imageReply)
	}

	u, a, imageReply = titleSeeds([]provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello!"},
	})
	if u != "hi" || a != "hello!" || imageReply {
		t.Fatalf("text reply: %q %q %v", u, a, imageReply)
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
