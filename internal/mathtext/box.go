package mathtext

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Box is the fixed-width character grid at the heart of the 2D display-math
// layout. Every subexpression — an atom, a fraction, a matrix — is a Box, and
// the whole formula is assembled by composing Boxes with the primitives below.
//
// The box model is a hand-rolled port of SymPy's stringPict/prettyForm box
// algorithm (sympy/printing/pretty/stringpict.py, BSD-3-Clause). No SymPy code
// is copied: only the geometry (the hconcat baseline alignment, the vstack
// centering, the drawn delimiters) was reimplemented in Go. Attribution and the
// upstream license are preserved for the borrowed algorithm.
//
// Invariants a valid Box maintains:
//
//   - every string in Lines has the SAME display width, equal to Width, where
//     display width is measured with rivo/uniseg (NOT len) so wide runes
//     (CJK inside \text{}, emoji) and combining sequences align exactly the way
//     they do everywhere else in the markdown renderer;
//   - Baseline is the row index (0-based, into Lines) of the reference line —
//     the row that sits on the surrounding text's baseline when this Box is
//     placed inline. For a plain one-line atom the baseline is 0.
//
// The zero Box (no lines) is a valid empty box of width 0 and height 0.
type Box struct {
	Lines    []string // equal display-width rows, top to bottom
	Baseline int      // row index of the reference line
	Width    int      // display width of every row (uniseg columns)
}

// boxWidth returns the monospace display width of s measured with uniseg — the
// single ruler the whole renderer shares. It is used instead of len(s) or the
// rune count so wide (CJK/emoji) and zero-width runes are accounted for.
func boxWidth(s string) int {
	return uniseg.StringWidth(s)
}

// pad right-pads line with spaces until its display width reaches want. A line
// already at or beyond want is returned unchanged (callers never pass a line
// wider than the box width, but the guard keeps pad total). Padding is added on
// the RIGHT only; left/both-side padding is done by the centering helper.
func pad(line string, want int) string {
	w := boxWidth(line)
	if w >= want {
		return line
	}
	return line + strings.Repeat(" ", want-w)
}

// blankLine returns a run of width spaces — a full-width empty row used to pad
// a box above or below its content during composition.
func blankLine(width int) string {
	if width <= 0 {
		return ""
	}
	return strings.Repeat(" ", width)
}

// NewBox builds a single-row Box from s with the baseline on that row (0). The
// row is stored verbatim and the width is its uniseg display width; s must not
// contain a newline (use NewBoxLines for multi-line text).
func NewBox(s string) Box {
	return Box{Lines: []string{s}, Baseline: 0, Width: boxWidth(s)}
}

// NewBoxLines builds a Box from multi-line text. The text is split on '\n',
// every row is right-padded to the widest row's display width, and the baseline
// is placed on the given row (clamped into range). An empty string yields a
// single empty row.
func NewBoxLines(text string, baseline int) Box {
	raw := strings.Split(text, "\n")
	width := 0
	for _, l := range raw {
		if w := boxWidth(l); w > width {
			width = w
		}
	}
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = pad(l, width)
	}
	if baseline < 0 {
		baseline = 0
	}
	if baseline >= len(lines) {
		baseline = len(lines) - 1
	}
	return Box{Lines: lines, Baseline: baseline, Width: width}
}

// EmptyBox returns a zero-size Box (no rows, width 0, baseline 0). It is the
// identity element for HConcat.
func EmptyBox() Box {
	return Box{Lines: nil, Baseline: 0, Width: 0}
}

// isEmptyBox reports whether a box carries no drawable content — used to skip
// omitted delimiter sides (an invisible "." or unknown symbol).
func isEmptyBox(b Box) bool {
	return b.Width == 0 || len(b.Lines) == 0
}

// Height returns the number of rows in the box.
func (b Box) Height() int { return len(b.Lines) }

// depth returns the number of rows BELOW the baseline (inclusive of none): the
// distance from the baseline row to the bottom edge. A one-row box has depth 0.
func (b Box) depth() int { return b.Height() - 1 - b.Baseline }

// String renders the box as its rows joined by newlines — the final block a
// caller emits. An empty box renders as "".
func (b Box) String() string {
	return strings.Join(b.Lines, "\n")
}

// HConcat places boxes side by side on a shared baseline and returns the joined
// Box. Boxes are aligned so their baseline rows land on the same output row:
//
//   - the result baseline is the max baseline across inputs (the tallest
//     ascent above the line);
//   - the result depth is the max depth across inputs (the deepest descent
//     below the line);
//   - each box is padded with blank rows ABOVE (to reach the common baseline)
//     and BELOW (to reach the common depth), then rows are concatenated
//     left-to-right.
//
// The result width is the sum of the input widths. Empty boxes contribute
// nothing. With no non-empty inputs the result is EmptyBox.
func HConcat(boxes ...Box) Box {
	// Collect the non-empty operands and the shared vertical extents.
	var parts []Box
	baseline := 0
	depth := 0
	for _, b := range boxes {
		if b.Height() == 0 || b.Width == 0 {
			continue
		}
		parts = append(parts, b)
		if b.Baseline > baseline {
			baseline = b.Baseline
		}
		if d := b.depth(); d > depth {
			depth = d
		}
	}
	if len(parts) == 0 {
		return EmptyBox()
	}
	height := baseline + depth + 1
	lines := make([]string, height)
	totalWidth := 0
	for _, b := range parts {
		above := baseline - b.Baseline // blank rows to add on top
		for row := 0; row < height; row++ {
			src := row - above
			if src >= 0 && src < b.Height() {
				lines[row] += b.Lines[src]
			} else {
				lines[row] += blankLine(b.Width)
			}
		}
		totalWidth += b.Width
	}
	return Box{Lines: lines, Baseline: baseline, Width: totalWidth}
}

// center returns line centered within width by padding it on both sides with
// spaces (extra odd column goes on the RIGHT, matching TeX's centering). Width
// is measured with uniseg. A line already at/over width is returned as-is.
func center(line string, width int) string {
	w := boxWidth(line)
	if w >= width {
		return line
	}
	total := width - w
	left := total / 2
	right := total - left
	return blankLine(left) + line + blankLine(right)
}

// VStack centers every box to the common (max) display width and stacks them
// top-to-bottom in the given order. The result baseline is baselineRow — the
// output row index that sits on the surrounding text's baseline (for a fraction
// this is the row of the drawn bar). baselineRow is clamped into range.
//
// This is the primitive behind fractions (numerator / bar / denominator with
// the baseline on the bar), \sum/\int limits, and matrix column stacking.
func VStack(boxes []Box, baselineRow int) Box {
	width := 0
	for _, b := range boxes {
		if b.Width > width {
			width = b.Width
		}
	}
	var lines []string
	for _, b := range boxes {
		for _, l := range b.Lines {
			lines = append(lines, center(l, width))
		}
	}
	if len(lines) == 0 {
		return EmptyBox()
	}
	if baselineRow < 0 {
		baselineRow = 0
	}
	if baselineRow >= len(lines) {
		baselineRow = len(lines) - 1
	}
	return Box{Lines: lines, Baseline: baselineRow, Width: width}
}

// Raise shifts the box's baseline UP by n rows (a smaller baseline index),
// which — once HConcat aligns everything on the shared baseline — lifts the box
// so it sits higher, as a superscript does. It adds no rows itself; the empty
// space is produced by HConcat when it pads to the common baseline. n is
// clamped so the baseline stays within the box.
func Raise(b Box, n int) Box {
	if b.Height() == 0 {
		return b
	}
	nb := b.Baseline - n
	if nb < 0 {
		nb = 0
	}
	if nb >= b.Height() {
		nb = b.Height() - 1
	}
	b.Baseline = nb
	return b
}

// Lower shifts the box's baseline DOWN by n rows (a larger baseline index),
// which drops the box so it sits lower, as a subscript does. Like Raise it adds
// no rows; HConcat supplies the padding. n is clamped into the box.
func Lower(b Box, n int) Box {
	return Raise(b, -n)
}

// Overline adds a DRAWN horizontal rule (─, U+2500) spanning the full width
// directly above the box — the vinculum for \sqrt and the bar for \bar/
// \overline. No combining marks are used: the rule is its own row of box-
// drawing glyphs. The baseline shifts down by one row so it still points at the
// same content line (a new row was inserted above every existing row).
func Overline(b Box) Box {
	if b.Height() == 0 {
		return NewBox("─")
	}
	lines := make([]string, 0, b.Height()+1)
	lines = append(lines, strings.Repeat("─", b.Width))
	lines = append(lines, b.Lines...)
	return Box{Lines: lines, Baseline: b.Baseline + 1, Width: b.Width}
}

// AccentRow adds a DRAWN accent row directly above the box, centered over its
// content — the terminal analogue of a TeX accent (\hat, \vec, \tilde, \dot,
// \ddot). glyph is a normal SPACING character (never a combining mark, per the
// package's hard rule): it is placed on its own new row, so the base rune is
// untouched. The baseline shifts down by one because a row was inserted above
// every existing row. A bar accent (\bar/\overline) uses Overline instead, since
// it spans the full width. An empty box yields just the accent glyph.
func AccentRow(b Box, glyph string) Box {
	if b.Height() == 0 {
		return NewBox(glyph)
	}
	// The accent may be wider than the base (\ddot's "··" over a 1-column atom):
	// widen the whole box to the max so every row keeps the box invariant, then
	// center both the accent and the (re-centered) base rows on that width.
	width := b.Width
	if gw := boxWidth(glyph); gw > width {
		width = gw
	}
	lines := make([]string, 0, b.Height()+1)
	lines = append(lines, center(glyph, width))
	for _, l := range b.Lines {
		lines = append(lines, center(l, width))
	}
	return Box{Lines: lines, Baseline: b.Baseline + 1, Width: width}
}

// HRule returns a one-row Box that is a full-width bar (─, U+2500) of the given
// width, with the baseline on that single row. It is the fraction bar: VStack
// the numerator, this rule, and the denominator with the baseline on the rule.
// A width <= 0 yields an empty box.
func HRule(width int) Box {
	if width <= 0 {
		return EmptyBox()
	}
	return Box{Lines: []string{strings.Repeat("─", width)}, Baseline: 0, Width: width}
}

// DelimKind identifies which bracket a tall delimiter draws.
type DelimKind int

const (
	DelimParen   DelimKind = iota // ( )
	DelimBracket                  // [ ]
	DelimBrace                    // { }
	DelimBar                      // | |  (also matrix determinant bars)
)

// delimGlyphs holds the drawn pieces for one side of a tall delimiter. Each is
// a single width-1 glyph; a box-drawing corner/hook top and bottom with an
// extension repeated between them, plus an optional middle piece (curly braces)
// placed on the center row. The single field is used when height == 1.
type delimGlyphs struct {
	single string // the height-1 form: ( ) [ ] { } |
	top    string // top corner/hook
	ext    string // extension piece repeated for every non-corner, non-mid row
	bottom string // bottom corner/hook
	mid    string // center piece (curly braces only); "" means none
}

// leftDelimGlyphs / rightDelimGlyphs are the verified box-drawing pieces per
// kind. Code points (all uniseg display width 1):
//
//	( left : ⎛ U+239B, ⎜ U+239C, ⎝ U+239D
//	) right: ⎞ U+239E, ⎟ U+239F, ⎠ U+23A0
//	[ left : ⎡ U+23A1, ⎢ U+23A2, ⎣ U+23A3
//	] right: ⎤ U+23A4, ⎥ U+23A5, ⎦ U+23A6
//	{ left : ⎧ U+23A7, ⎨ U+23A8 (mid), ⎩ U+23A9, ⎪ U+23AA (ext)
//	} right: ⎫ U+23AB, ⎬ U+23AC (mid), ⎭ U+23AD, ⎪ U+23AA (ext)
//	| both : │ U+2502
var leftDelimGlyphs = map[DelimKind]delimGlyphs{
	DelimParen:   {single: "(", top: "⎛", ext: "⎜", bottom: "⎝"},
	DelimBracket: {single: "[", top: "⎡", ext: "⎢", bottom: "⎣"},
	DelimBrace:   {single: "{", top: "⎧", ext: "⎪", bottom: "⎩", mid: "⎨"},
	DelimBar:     {single: "│", top: "│", ext: "│", bottom: "│"},
}

var rightDelimGlyphs = map[DelimKind]delimGlyphs{
	DelimParen:   {single: ")", top: "⎞", ext: "⎟", bottom: "⎠"},
	DelimBracket: {single: "]", top: "⎤", ext: "⎥", bottom: "⎦"},
	DelimBrace:   {single: "}", top: "⎫", ext: "⎪", bottom: "⎭", mid: "⎬"},
	DelimBar:     {single: "│", top: "│", ext: "│", bottom: "│"},
}

// LeftDelim draws the left half of a tall bracket of the given kind as a
// height-tall, width-1 Box with its baseline on the given row. When height ==
// 1 the single-glyph form is used ((, [, {, │). When height >= 2 the top and
// bottom corner/hook glyphs cap an extension run; curly braces additionally
// place their middle piece on the vertical center row. baseline is clamped into
// range.
func LeftDelim(kind DelimKind, height, baseline int) Box {
	return delimBox(leftDelimGlyphs[kind], height, baseline)
}

// RightDelim draws the right half of a tall bracket — the mirror of LeftDelim
// with the right-facing glyphs (), ], }, │).
func RightDelim(kind DelimKind, height, baseline int) Box {
	return delimBox(rightDelimGlyphs[kind], height, baseline)
}

// delimBox assembles a delimiter column from its glyph set. The curly-brace
// middle piece lands on the vertical center row ((height-1)/2); every other
// interior row is the extension glyph.
func delimBox(g delimGlyphs, height, baseline int) Box {
	if height < 1 {
		height = 1
	}
	if height == 1 {
		if baseline < 0 {
			baseline = 0
		}
		return Box{Lines: []string{g.single}, Baseline: 0, Width: 1}
	}
	lines := make([]string, height)
	center := (height - 1) / 2
	for row := 0; row < height; row++ {
		switch {
		case row == 0:
			lines[row] = g.top
		case row == height-1:
			lines[row] = g.bottom
		case g.mid != "" && row == center:
			lines[row] = g.mid
		default:
			lines[row] = g.ext
		}
	}
	if baseline < 0 {
		baseline = 0
	}
	if baseline >= height {
		baseline = height - 1
	}
	return Box{Lines: lines, Baseline: baseline, Width: 1}
}
