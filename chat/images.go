package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chatchain/internal/imgterm"
	"chatchain/provider"
)

// imagesDir is where generated images land as plain files the user can grab:
// ~/.chatchain/images (session bundles additionally persist them as message
// attachments for lossless resume and history round-trips).
func imagesDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".chatchain", "images")
	return dir, os.MkdirAll(dir, 0o755)
}

// imageExt maps a mime type to a file extension.
func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "bin"
}

// saveImage writes one generated image and returns its path. The name embeds
// a timestamp and index so parallel generations never collide.
func saveImage(att provider.Attachment, seq int) (string, error) {
	dir, err := imagesDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.%s", time.Now().Format("20060102-150405"), seq, imageExt(att.MimeType))
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, att.Data, 0o644)
}

// imageMaxCols / imageMaxRows cap the glyph render size — about a third of
// a roomy terminal, never the whole screen. The live terminal width still
// wins when narrower.
const (
	imageMaxCols = 72
	imageMaxRows = 14
)

// hasImages reports whether the provider generated images this stream — an
// image-only response carries no text, which must not read as an EOF error.
func hasImages(p any) bool {
	ip, ok := p.(provider.ImageOutputProvider)
	return ok && len(ip.LastImages()) > 0
}

// collectImages drains the provider's generated images after a stream round:
// they are attached to the assistant message (history round-trip + session
// persistence), saved under ~/.chatchain/images, and rendered into the chat
// as half-block blocks. A decode failure still saves the file — the path
// line is the fallback rendering.
func collectImages(p any, tr *transcript, width func() int, msg *provider.Message) {
	ip, ok := p.(provider.ImageOutputProvider)
	if !ok {
		return
	}
	images := ip.LastImages()
	if len(images) == 0 {
		return
	}
	for i, att := range images {
		path, err := saveImage(att, i)
		if err != nil {
			tr.error("Saving image failed: %v", err)
			continue
		}
		att.Filename = filepath.Base(path)
		msg.Attachments = append(msg.Attachments, att)

		maxCols := imageMaxCols
		if w := width(); w-2 < maxCols {
			maxCols = w - 2
		}
		rows, rerr := imgterm.Render(att.Data, maxCols, imageMaxRows)
		caption := fmt.Sprintf("🖼 saved: %s", path)
		if rerr != nil {
			tr.notice("%s (%s)", caption, strings.TrimPrefix(rerr.Error(), "decode image: "))
			continue
		}
		tr.image(rows, caption)
	}
}

// SaveImagesQuiet is the non-interactive (-m) tail: generated images are
// saved and their paths printed as plain lines — no ANSI rasterizing into a
// pipe. Mid-round images in a tool loop are out of scope; the final response
// (the image-model shape) is what single-shot runs care about.
func SaveImagesQuiet(p any, w io.Writer) {
	ip, ok := p.(provider.ImageOutputProvider)
	if !ok {
		return
	}
	for i, att := range ip.LastImages() {
		path, err := saveImage(att, i)
		if err != nil {
			fmt.Fprintf(w, "saving image failed: %v\n", err)
			continue
		}
		fmt.Fprintf(w, "🖼 saved: %s\n", path)
	}
}
