// Package imgterm rasterizes images into self-contained ANSI half-block
// lines for the terminal scrollback: each character cell is one column wide
// and two image rows tall (▀ paints the top pixel as foreground, the bottom
// as background, both 24-bit). Every returned line carries its own SGR state
// and ends reset — the shape the chat transcript and the staging window
// demand of committed rows.
package imgterm

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// upperHalf paints fg on top, bg below — one cell, two stacked pixels.
const upperHalf = "▀"

// Render decodes data and rasterizes it to at most maxCols columns and
// maxRows cell rows (aspect preserved; a cell is a 1:2 box, so one row is
// two image pixels tall). The tighter of the two constraints wins — a
// generated image must never take over the screen. Images already smaller
// render 1:1. The error names the decode failure — callers fall back to
// just saving the file.
func Render(data []byte, maxCols, maxRows int) ([]string, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return RenderImage(img, maxCols, maxRows), nil
}

// RenderImage rasterizes an already-decoded image.
func RenderImage(img image.Image, maxCols, maxRows int) []string {
	if maxCols < 1 {
		maxCols = 1
	}
	if maxRows < 1 {
		maxRows = 1
	}
	srcW := img.Bounds().Dx()
	srcH := img.Bounds().Dy()
	if srcW == 0 || srcH == 0 {
		return nil
	}
	maxPxH := maxRows * 2
	w := srcW
	if w > maxCols {
		w = maxCols
	}
	h := (srcH*w + srcW/2) / srcW
	if h > maxPxH {
		// Height-bound: shrink width to keep the aspect.
		h = maxPxH
		w = (srcW*h + srcH/2) / srcH
		if w < 1 {
			w = 1
		}
	}
	if h < 1 {
		h = 1
	}
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Over, nil)

	lines := make([]string, 0, (h+1)/2)
	var b strings.Builder
	for y := 0; y < h; y += 2 {
		b.Reset()
		for x := 0; x < w; x++ {
			tr, tg, tb := blendOnBlack(scaled.RGBAAt(x, y))
			if y+1 < h {
				br, bg_, bb := blendOnBlack(scaled.RGBAAt(x, y+1))
				fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm%s", tr, tg, tb, br, bg_, bb, upperHalf)
			} else {
				// Odd final row: only the top half is image; the bottom keeps
				// the terminal's own background.
				fmt.Fprintf(&b, "\x1b[49m\x1b[38;2;%d;%d;%dm%s", tr, tg, tb, upperHalf)
			}
		}
		b.WriteString("\x1b[0m")
		lines = append(lines, b.String())
	}
	return lines
}

// blendOnBlack composites a premultiplied RGBA pixel over black — terminals
// have no alpha channel, and dark blends degrade most gracefully across
// light and dark themes.
func blendOnBlack(c interface{ RGBA() (r, g, b, a uint32) }) (uint8, uint8, uint8) {
	r, g, b, _ := c.RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
}
