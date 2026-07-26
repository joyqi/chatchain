package chat

import (
	"bytes"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chatchain/internal/imgterm"
	"chatchain/internal/markdown"
	"chatchain/provider"
)

// imageChoice is one candidate on the bare-/edit picker: an image the model
// generated in this session, with the prompt that produced it.
type imageChoice struct {
	att    provider.Attachment
	prompt string // the user message of the round that produced it
	at     time.Time
}

// generatedImageChoices collects every model-generated image in history,
// NEWEST FIRST. Only assistant attachments qualify — a user attachment is
// something the user supplied (or an /edit canvas copy), not a generation.
// The prompt of the round is carried along as the label's main text.
func generatedImageChoices(history []provider.Message) []imageChoice {
	var out []imageChoice
	prompt := ""
	for _, m := range history {
		switch m.Role {
		case "user":
			prompt = m.Content
		case "assistant":
			for _, att := range m.Attachments {
				if !strings.HasPrefix(att.MimeType, "image/") || len(att.Data) == 0 {
					continue
				}
				out = append(out, imageChoice{att: att, prompt: flattenLine(prompt), at: imageTime(att.Filename)})
			}
		}
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// flattenLine folds text into ONE line: newlines and tabs become spaces,
// runs of whitespace collapse, control characters go. Anything rendered as a
// single row needs this — a picker label spanning rows breaks the surface's
// one-item-one-row bookkeeping, and a session title spanning rows breaks the
// picker and the window title alike.
func flattenLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// imageTime recovers the generation time from a saved image's name
// ("20260725-110419-0.png"); a zero time means "unknown" and the label just
// drops the time prefix.
func imageTime(filename string) time.Time {
	base := filepath.Base(filename)
	if len(base) < 15 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102-150405", base[:15], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// imageChoiceLabels renders the picker rows: "HH:MM · <prompt>". The label
// is NOT truncated here — the picker knows its column budget and truncates by
// display width, so a narrow terminal (no preview pane) still gets long text.
func imageChoiceLabels(choices []imageChoice) []string {
	labels := make([]string, len(choices))
	for i, c := range choices {
		text := c.prompt
		if text == "" {
			text = c.att.Filename
		}
		if c.at.IsZero() {
			labels[i] = text
			continue
		}
		labels[i] = c.at.Format("15:04") + " · " + text
	}
	return labels
}

// imageChoiceDetails builds the per-item detail line: the on-disk path as an
// OSC 8 link. The DISPLAY text is shortened to fit width while the link
// target stays the full absolute path — so the row never wraps and ⌘-click
// still opens the original. Files no longer on disk get no link.
func imageChoiceDetails(choices []imageChoice, imgDir string, width int) []string {
	details := make([]string, len(choices))
	for i, c := range choices {
		if imgDir == "" || c.att.Filename == "" {
			continue
		}
		path := filepath.Join(imgDir, c.att.Filename)
		if !fileExists(path) {
			continue
		}
		details[i] = "🖼 " + markdown.Hyperlink("file://"+path, shortenPath(path, width-4))
	}
	return details
}

// shortenPath fits a path into width display columns: "~" for the home
// directory first, then the file name alone. Truncation happens BEFORE the
// text is wrapped in a hyperlink — slicing an OSC 8 sequence would corrupt it.
func shortenPath(path string, width int) string {
	if width < 8 {
		width = 8
	}
	display := path
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		display = "~" + path[len(home):]
	}
	if displayWidth(display) <= width {
		return display
	}
	if base := filepath.Base(path); displayWidth(base) <= width {
		return base
	}
	return truncateRunes(filepath.Base(path), width)
}

// imagePreviewer renders picker previews, decoding each image at most once
// (a multi-MB PNG decode per keystroke would be visible lag); the cached
// image.Image is re-rasterized when the pane geometry changes.
type imagePreviewer struct {
	choices []imageChoice
	decoded map[int]image.Image
	failed  map[int]bool
}

func newImagePreviewer(choices []imageChoice) *imagePreviewer {
	return &imagePreviewer{choices: choices, decoded: map[int]image.Image{}, failed: map[int]bool{}}
}

// render is the ui.Panel.Preview callback.
func (p *imagePreviewer) render(index, maxCols, maxRows int) []string {
	if index < 0 || index >= len(p.choices) || p.failed[index] {
		return nil
	}
	img, ok := p.decoded[index]
	if !ok {
		decoded, _, err := image.Decode(bytes.NewReader(p.choices[index].att.Data))
		if err != nil {
			p.failed[index] = true
			return []string{fmt.Sprintf("(cannot preview %s)", p.choices[index].att.Filename)}
		}
		p.decoded[index] = decoded
		img = decoded
	}
	return imgterm.RenderImage(img, maxCols, maxRows)
}
