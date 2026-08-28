package chat

import (
	"testing"

	"github.com/joyqi/iota/provider"
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

// /redo re-sends the last REQUEST: lastUserMessage is what it recovers, so
// rewording works from the canvas that produced the rejected result — not
// from that result (which is what another /edit would grab).
func TestLastUserMessage(t *testing.T) {
	canvas := provider.Attachment{Filename: "gen-1.png", MimeType: "image/png", Data: []byte{1}}
	bad := provider.Attachment{Filename: "gen-2.png", MimeType: "image/png", Data: []byte{2}}

	if got := lastUserMessage(nil); got != nil {
		t.Fatalf("empty history: %v", got)
	}
	if got := lastUserMessage([]provider.Message{{Role: "assistant", Content: "hi"}}); got != nil {
		t.Fatalf("no user turn: %v", got)
	}

	history := []provider.Message{
		{Role: "user", Content: "a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{canvas}},
		{Role: "user", Content: "add a heart", Attachments: []provider.Attachment{canvas}}, // the /edit turn
		{Role: "assistant", Attachments: []provider.Attachment{bad}},                       // the rejected result
	}
	got := lastUserMessage(history)
	if got == nil || got.Content != "add a heart" {
		t.Fatalf("recovered request = %+v", got)
	}
	// The reference is the ORIGINAL canvas, never the rejected output.
	if len(got.Attachments) != 1 || got.Attachments[0].Filename != "gen-1.png" {
		t.Fatalf("references = %+v, want the canvas that produced the result", got.Attachments)
	}
	// Contrast: /edit would take the rejected image as its canvas.
	if refs := lastGeneratedImages(history); len(refs) != 1 || refs[0].Filename != "gen-2.png" {
		t.Fatalf("sanity: /edit canvas = %+v", refs)
	}
}
