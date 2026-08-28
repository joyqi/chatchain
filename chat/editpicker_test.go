package chat

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	// Large enough that the pane geometry, not the source size, bounds the
	// render (imgterm never upscales).
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = 0xcc
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The picker lists model-generated images newest first, carrying each round's
// prompt; user attachments (uploads and /edit canvas copies) never qualify.
func TestGeneratedImageChoices(t *testing.T) {
	data := testPNG(t)
	img := func(name string) provider.Attachment {
		return provider.Attachment{Filename: name, MimeType: "image/png", Data: data}
	}
	history := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a cat\nsecond line"},
		{Role: "assistant", Attachments: []provider.Attachment{img("20260725-093820-0.png")}},
		{Role: "user", Content: "add a hat", Attachments: []provider.Attachment{img("20260725-093820-0.png")}}, // canvas copy
		{Role: "assistant", Attachments: []provider.Attachment{img("20260725-101500-0.png")}},
		{Role: "user", Content: "look at this", Attachments: []provider.Attachment{img("upload.png")}},
		{Role: "assistant", Content: "text only"},
	}

	choices := generatedImageChoices(history)
	if len(choices) != 2 {
		t.Fatalf("choices = %d, want the 2 generated images", len(choices))
	}
	if choices[0].att.Filename != "20260725-101500-0.png" {
		t.Fatalf("newest must come first, got %q", choices[0].att.Filename)
	}
	if choices[0].prompt != "add a hat" || choices[1].prompt != "a cat second line" {
		t.Fatalf("prompts wrong: %q / %q", choices[0].prompt, choices[1].prompt)
	}

	labels := imageChoiceLabels(choices)
	if !strings.HasPrefix(labels[0], "10:15 · add a hat") {
		t.Fatalf("label = %q", labels[0])
	}
	// Labels stay ONE line: a multi-row label would break the surface's
	// one-item-one-row bookkeeping.
	for _, l := range labels {
		if strings.ContainsAny(l, "\n\r\t") {
			t.Fatalf("label spans rows: %q", l)
		}
	}
}

// Details link the on-disk file with a shortened display text; a missing file
// yields no link at all (never a dead one).
func TestImageChoiceDetails(t *testing.T) {
	dir := t.TempDir()
	choices := []imageChoice{
		{att: provider.Attachment{Filename: "here.png", MimeType: "image/png"}},
		{att: provider.Attachment{Filename: "gone.png", MimeType: "image/png"}},
	}
	if err := os.WriteFile(filepath.Join(dir, "here.png"), []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}

	details := imageChoiceDetails(choices, dir, 100)
	if !strings.Contains(details[0], "here.png") {
		t.Fatalf("existing file must get a detail line: %q", details[0])
	}
	if details[1] != "" {
		t.Fatalf("missing file must get no link: %q", details[1])
	}

	// A narrow width falls back to the file name — the display text is sized
	// BEFORE the hyperlink wraps it, so the escape is never sliced.
	narrow := imageChoiceDetails(choices, dir, 14)
	if !strings.Contains(narrow[0], "here.png") || strings.Contains(narrow[0], dir) {
		t.Fatalf("narrow detail = %q, want the bare name", narrow[0])
	}
}

// The previewer decodes each image once and re-rasterizes per geometry.
func TestImagePreviewerCachesDecode(t *testing.T) {
	choices := []imageChoice{
		{att: provider.Attachment{Filename: "ok.png", MimeType: "image/png", Data: testPNG(t)}},
		{att: provider.Attachment{Filename: "broken.png", MimeType: "image/png", Data: []byte{1, 2}}},
	}
	p := newImagePreviewer(choices)

	rows := p.render(0, 20, 6)
	if len(rows) == 0 || !strings.Contains(strings.Join(rows, ""), "▀") {
		t.Fatalf("preview rows = %v", rows)
	}
	if _, ok := p.decoded[0]; !ok {
		t.Fatal("decoded image not cached")
	}
	if wider := p.render(0, 40, 12); len(wider) <= len(rows) {
		t.Fatalf("larger geometry must produce more rows: %d vs %d", len(wider), len(rows))
	}

	if got := p.render(1, 20, 6); len(got) != 1 || !strings.Contains(got[0], "cannot preview") {
		t.Fatalf("undecodable image = %v", got)
	}
	if !p.failed[1] {
		t.Fatal("a failed decode must be remembered, not retried each frame")
	}
	if got := p.render(99, 20, 6); got != nil {
		t.Fatalf("out-of-range index = %v", got)
	}
}
