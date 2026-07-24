package chat

import (
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
