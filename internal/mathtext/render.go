package mathtext

// This file is the public surface of the package, gathering the Phase 1 inline
// API and the Phase 2 signatures so the markdown writer can be wired against the
// final API today. The inline entry point lives in inline.go (ApproxInline);
// the delimiter recognizers live in delim.go (FindInline, IsInlineOpen,
// IsDisplayFence, StripDelimiters, CleanSource).

// Render2D lays out display math into a multi-line block that fits within width
// columns, returning ok=true on success. It is the Phase 2 API: the parser and
// the 2D box engine are not implemented yet, so it currently returns ("",
// false) for every input, signalling the caller to fall back to the cleaned
// linear source. The signature is fixed now so the display-block wiring in
// chat/markdown.go can be written against it and light up when P2 lands.
//
// P2: implement the recursive-descent parser + stringPict box model here.
func Render2D(latex string, width int) (block string, ok bool) {
	_ = latex
	_ = width
	return "", false
}
