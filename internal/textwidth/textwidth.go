// Package textwidth is the single home of terminal display-width measurement.
// Every package (chat, markdown, ui) measures through these two functions —
// never hand-roll CJK/emoji width ranges, and never mix rulers: uniseg for
// strings, go-runewidth for single runes (see each function for why).
package textwidth

import (
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// StringWidth returns the terminal display width of a string. It measures
// uniseg grapheme clusters, which is sequence-aware: a VS16 emoji such as
// "⚖️" (U+2696 U+FE0F) counts as 2 columns — matching how terminals render
// it — and combining marks and ZWJ sequences collapse into their cluster.
// This is the same measurement lipgloss uses, so tables built from these
// widths stay aligned. Use it for any whole string; see RuneWidth for the
// single-rune seam.
func StringWidth(s string) int {
	return uniseg.StringWidth(s)
}

// RuneWidth returns the display width of a single rune. A lone rune has no
// sequence context (no following VS16, ZWJ, or combining mark), so grapheme
// segmentation cannot apply and go-runewidth's per-rune tables are the right
// tool. This is the seam injected into components that walk text one rune at
// a time.
func RuneWidth(r rune) int {
	return runewidth.RuneWidth(r)
}
