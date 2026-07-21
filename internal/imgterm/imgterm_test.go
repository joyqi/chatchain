package imgterm

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// encode builds a PNG from explicit pixels.
func encode(t *testing.T, w, h int, px func(x, y int) color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, px(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A 2×2 image renders as ONE line of two half-block cells, each cell's fg the
// top pixel and bg the bottom pixel, line reset-terminated.
func TestRenderHalfBlocks(t *testing.T) {
	data := encode(t, 2, 2, func(x, y int) color.RGBA {
		if y == 0 {
			return color.RGBA{255, 0, 0, 255} // top row red
		}
		return color.RGBA{0, 0, 255, 255} // bottom row blue
	})
	lines, err := Render(data, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	want := "\x1b[38;2;255;0;0m\x1b[48;2;0;0;255m▀\x1b[38;2;255;0;0m\x1b[48;2;0;0;255m▀\x1b[0m"
	if lines[0] != want {
		t.Fatalf("line:\n%q\nwant:\n%q", lines[0], want)
	}
}

// Wide images downscale to maxCols with aspect kept; rows = ceil(h/2).
func TestRenderScalesToMaxCols(t *testing.T) {
	data := encode(t, 200, 100, func(x, y int) color.RGBA { return color.RGBA{9, 9, 9, 255} })
	lines, err := Render(data, 40, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 { // 100 * 40/200 = 20 px tall → 10 cell rows
		t.Fatalf("rows = %d, want 10", len(lines))
	}
	// Self-contained: every line ends with a full reset.
	for i, ln := range lines {
		if !strings.HasSuffix(ln, "\x1b[0m") {
			t.Fatalf("line %d not reset-terminated", i)
		}
	}
}

// An odd final pixel row paints only foregrounds (terminal bg below).
func TestRenderOddHeight(t *testing.T) {
	data := encode(t, 1, 3, func(x, y int) color.RGBA { return color.RGBA{1, 2, 3, 255} })
	lines, err := Render(data, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("rows = %d, want 2", len(lines))
	}
	if !strings.Contains(lines[1], "\x1b[49m") || strings.Contains(lines[1], "\x1b[48;2") {
		t.Fatalf("odd row must keep the terminal background: %q", lines[1])
	}
}

func TestRenderBadData(t *testing.T) {
	if _, err := Render([]byte("not an image"), 80, 24); err == nil {
		t.Fatal("want a decode error")
	}
}

// A tall image is height-bound: rows never exceed maxRows and the width
// shrinks to keep the aspect — a generated portrait must not take the screen.
func TestRenderHeightCap(t *testing.T) {
	data := encode(t, 100, 400, func(x, y int) color.RGBA { return color.RGBA{5, 5, 5, 255} })
	lines, err := Render(data, 72, 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) > 14 {
		t.Fatalf("rows = %d, want ≤ 14", len(lines))
	}
	// 28 px tall → width 100*28/400 = 7 cells.
	if got := strings.Count(lines[0], "▀"); got != 7 {
		t.Fatalf("cols = %d, want 7", got)
	}
}
