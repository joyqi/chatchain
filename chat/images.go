package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joyqi/iota/internal/app"
	"github.com/joyqi/iota/internal/imgterm"
	"github.com/joyqi/iota/internal/markdown"
	"github.com/joyqi/iota/internal/ui"
	"github.com/joyqi/iota/provider"
)

// fallbackImagesDir holds generated images for EPHEMERAL sessions
// (~/.iota/images); saved sessions keep them inside the bundle
// (SessionWriter.ImagesDir) so deleting the session deletes its images.
func fallbackImagesDir() (string, error) {
	home, err := app.Home()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "images")
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

// saveImage writes one generated image into dir ("" = the ephemeral
// fallback) and returns its path. The name embeds a timestamp and index so
// parallel generations never collide.
func saveImage(att provider.Attachment, dir string, seq int) (string, error) {
	if dir == "" {
		var err error
		if dir, err = fallbackImagesDir(); err != nil {
			return "", err
		}
	}
	name := fmt.Sprintf("%s-%d.%s", time.Now().Format("20060102-150405"), seq, imageExt(att.MimeType))
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, att.Data, 0o644)
}

// imageMaxCols / imageMaxRows cap the glyph render size — about a third of
// a roomy terminal, never the whole screen. The live terminal width still
// wins when narrower.
const (
	imageMaxCols    = 72
	imageMaxRows    = 14
	imageIndentCols = 2 // uniform left indent for image rows and caption
)

// hasImages reports whether the provider generated images this stream — an
// image-only response carries no text, which must not read as an EOF error.
func hasImages(p any) bool {
	ip, ok := p.(provider.ImageOutputProvider)
	return ok && len(ip.LastImages()) > 0
}

// lastUserMessage returns the most recent user message — the request /redo
// re-sends. Its attachments are the references that produced the last
// result, so re-running works from the SAME canvas rather than from the
// output the user just rejected.
func lastUserMessage(history []provider.Message) *provider.Message {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return &history[i]
		}
	}
	return nil
}

// lastGeneratedImages returns the image attachments of the most recent
// assistant reply that carried any — the canvas the /edit command re-sends
// as this turn's reference images.
func lastGeneratedImages(history []provider.Message) []provider.Attachment {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		var imgs []provider.Attachment
		for _, a := range history[i].Attachments {
			if strings.HasPrefix(a.MimeType, "image/") {
				imgs = append(imgs, a)
			}
		}
		if len(imgs) > 0 {
			return imgs
		}
	}
	return nil
}

// collectImages drains the provider's generated images after a stream round:
// they are attached to the assistant message (history round-trip + session
// persistence), saved under ~/.iota/images, and rendered into the chat
// as half-block blocks. A decode failure still saves the file — the path
// line is the fallback rendering.
// dir resolves lazily — only when an image actually needs saving — so the
// session bundle's lazy-creation contract survives image-less turns.
func collectImages(p any, tr *transcript, width func() int, dir func() string, msg *provider.Message) {
	ip, ok := p.(provider.ImageOutputProvider)
	if !ok {
		return
	}
	images := ip.LastImages()
	if len(images) == 0 {
		return
	}
	target := dir()
	for i, att := range images {
		path, err := saveImage(att, target, i)
		if err != nil {
			tr.error("Saving image failed: %v", err)
			continue
		}
		att.Filename = filepath.Base(path)
		msg.Attachments = append(msg.Attachments, att)

		maxCols := imageMaxCols
		if w := width(); w-2-imageIndentCols < maxCols {
			maxCols = w - 2 - imageIndentCols
		}
		rows, rerr := imgterm.Render(att.Data, maxCols, imageMaxRows)
		caption := "🖼 saved: " + markdown.Hyperlink("file://"+path, path)
		if rerr != nil {
			tr.notice("%s (%s)", caption, strings.TrimPrefix(rerr.Error(), "decode image: "))
			continue
		}
		tr.image(rows, caption)
	}
}

// saveImagesQuiet is the non-interactive (-m) tail: generated images are
// saved and their paths RETURNED rather than printed — no ANSI rasterizing
// into a pipe, and no writing either, because the two output formats spend
// them differently (text prints lines, JSON files them under images /
// image_errors). Mid-round images in a tool loop are out of scope; the final
// response (the image-model shape) is what single-shot runs care about.
func saveImagesQuiet(p any) (paths, failures []string) {
	ip, ok := p.(provider.ImageOutputProvider)
	if !ok {
		return nil, nil
	}
	for i, att := range ip.LastImages() {
		path, err := saveImage(att, "", i)
		if err != nil {
			failures = append(failures, fmt.Sprintf("saving image failed: %v", err))
			continue
		}
		paths = append(paths, path)
	}
	return paths, failures
}

// imagePartialObserver is the optional provider seam for progressive
// image-generation frames (openresponses partial_image events).
type imagePartialObserver interface {
	SetImagePartialObserver(fn func(data []byte))
}

// watchImagePartials renders each progressive frame as a tiny half-block
// thumbnail into the generation widget's body — the widget the composing
// observer raised morphs from spinner-only to a refining preview, and the
// final image replaces it in place. Runs on the stream goroutine; the ui
// facade is safe for concurrent use.
// raise, when non-nil, is called before the first frame: a dedicated image
// provider's turn has no tool-call widget to fill, so it opens one (the
// transcript's own openCall, which the final tr.image then morphs in place).
// Tool-driven generation passes nil — its widget is already up.
func watchImagePartials(u *ui.UI, tp any, raise func()) func() {
	obs, ok := tp.(imagePartialObserver)
	if !ok {
		return func() {}
	}
	var once sync.Once
	obs.SetImagePartialObserver(func(data []byte) {
		if raise != nil {
			once.Do(raise)
		}
		// Partial frames are full-resolution, low-DETAIL images; render at
		// about half the final block size so the composition is actually
		// visible while it refines (frame growth is safe — only shrinks
		// bounce the composer, and the final image is larger still).
		rows, err := imgterm.Render(data, 64, 12)
		if err != nil {
			return
		}
		u.CallBody(rows)
	})
	return func() { obs.SetImagePartialObserver(nil) }
}
