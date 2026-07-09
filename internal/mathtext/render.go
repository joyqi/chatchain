package mathtext

// This file is the public surface of the package, gathering the Phase 1 inline
// API and the Phase 2 signatures so the markdown writer can be wired against the
// final API today. The inline entry point lives in inline.go (ApproxInline);
// the delimiter recognizers live in delim.go (FindInline, IsInlineOpen,
// IsDisplayFence, StripDelimiters, CleanSource).

// Render2D lays out display math into a multi-line block, returning ok=true on
// success. It parses the LaTeX (parse.go) and lays the AST out with the
// stringPict box model (box.go/layout.go). On a parse or layout failure — an
// unknown macro, a malformed group, any Tier-3 construct — it returns the
// cleaned linear source with ok=false so the caller falls back gracefully.
//
// The width argument is the caller's target column budget. A laid-out block
// that exceeds width is STILL returned with ok=true: shrinking/scrolling a very
// wide formula is a later phase; best-effort overflow is preferred to dropping
// to linear source. width is currently advisory (no wrapping is done) and kept
// in the signature for that future phase.
//
// The returned block never contains a Unicode combining mark (U+0300–U+036F):
// the layout draws every bar, vinculum, and tall delimiter from box-drawing
// glyphs. As a defensive guard, a block that somehow carried one falls back to
// the cleaned source rather than emit it.
func Render2D(latex string, width int) (block string, ok bool) {
	_ = width // advisory; overflow is best-effort for now (see doc above)
	node, err := Parse(latex)
	if err != nil {
		return CleanSource(latex), false
	}
	b := layout(node)
	out := b.String()
	if hasCombiningMark(out) {
		// Should be unreachable — the layout is combining-mark-free by
		// construction — but never leak one into the terminal.
		return CleanSource(latex), false
	}
	return out, true
}
