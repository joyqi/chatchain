package chat

import (
	"testing"

	"chatchain/provider"
)

// lastGeneratedImages picks the /edit canvas: the newest assistant reply
// carrying image attachments, skipping text-only replies in between.
func TestLastGeneratedImages(t *testing.T) {
	img1 := provider.Attachment{Filename: "image-1.png", MimeType: "image/png", Data: []byte{1}}
	img2 := provider.Attachment{Filename: "image-2.png", MimeType: "image/png", Data: []byte{2}}

	if got := lastGeneratedImages(nil); got != nil {
		t.Fatalf("empty history: %v", got)
	}
	if got := lastGeneratedImages([]provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}); got != nil {
		t.Fatalf("text-only history: %v", got)
	}

	history := []provider.Message{
		{Role: "user", Content: "a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{img1}},
		{Role: "user", Content: "a dog"},
		{Role: "assistant", Attachments: []provider.Attachment{img2}},
		{Role: "user", Content: "thanks"},
		{Role: "assistant", Content: "you're welcome"}, // text reply doesn't shadow the canvas
	}
	got := lastGeneratedImages(history)
	if len(got) != 1 || got[0].Filename != "image-2.png" {
		t.Fatalf("canvas = %v, want the newest generated image", got)
	}

	// Non-image attachments never qualify as a canvas.
	if got := lastGeneratedImages([]provider.Message{
		{Role: "assistant", Attachments: []provider.Attachment{{Filename: "notes.txt", MimeType: "text/plain", Data: []byte("x")}}},
	}); got != nil {
		t.Fatalf("non-image attachment treated as canvas: %v", got)
	}
}
