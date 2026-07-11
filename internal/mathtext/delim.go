package mathtext

import "strings"

// Delimiter recognition and body extraction for the four math forms models
// emit:
//
//	inline:  $...$   and  \(...\)
//	display: $$...$$ and  \[...\]
//
// Phase 1 only USES the inline recognizers (wired into the markdown inline
// path), but the display recognizers are defined here too so the Phase 2
// display-block engine reuses the same normalization surface. All helpers are
// pure string functions with no terminal dependency.

// InlineDelimiter describes one matched inline span within a text run: the
// byte offsets of the whole delimited span (start inclusive, end exclusive) and
// the inner math body with the delimiters stripped.
type InlineDelimiter struct {
	Start int    // byte offset of the opening delimiter
	End   int    // byte offset just past the closing delimiter
	Body  string // inner math body, delimiters removed
}

// FindInline scans src starting at byte offset from for the next inline math
// span, honoring escapes and the currency guard. It returns the matched span
// and ok=true, or ok=false when no inline math begins at/after from.
//
// Recognized openers:
//
//	\(  ... \)   — always math (an explicit LaTeX delimiter)
//	$   ... $    — math UNLESS it is an escaped \$ or looks like currency
//	               ("$5", "$ ", or an unmatched trailing $)
//
// A "$" is treated as currency (and skipped) when the character immediately
// after it is an ASCII space, or when no unescaped closing "$" follows on the
// same run. A digit right after the opener no longer disqualifies it — real
// formulas like "$1/\pi$" start with a digit — so the "it costs $5 to $10"
// case is ruled out at the CLOSING "$" instead (scanDollarMath refuses to
// close on a "$" immediately followed by a digit). "$$" is a display fence and
// is not matched here.
func FindInline(src string, from int) (InlineDelimiter, bool) {
	for i := from; i < len(src); i++ {
		switch src[i] {
		case '\\':
			// \( ... \) explicit inline math.
			if i+1 < len(src) && src[i+1] == '(' {
				if body, end, ok := scanParenMath(src, i+2); ok {
					return InlineDelimiter{Start: i, End: end, Body: body}, true
				}
			}
			// Any other backslash escape (\$, \\, \[, …) is skipped: advance past
			// the escaped rune so a literal \$ can never open a math span.
			i++
			continue
		case '$':
			if i+1 < len(src) && src[i+1] == '$' {
				// Display fence "$$": not inline; skip both dollars.
				i++
				continue
			}
			if isCurrencyDollar(src, i) {
				continue
			}
			if body, end, ok := scanDollarMath(src, i+1); ok {
				return InlineDelimiter{Start: i, End: end, Body: body}, true
			}
			// No valid close on this run: treat the lone $ as literal text.
			continue
		}
	}
	return InlineDelimiter{}, false
}

// isCurrencyDollar reports whether the '$' at byte offset i cannot open an
// inline math span: it is a trailing lone '$' or is immediately followed by a
// space ("$ 5"). A digit right after it is allowed — "$1/\pi$" is a valid
// formula — so the currency case "$5 or $10" is ruled out at the closing '$'
// (see scanDollarMath) rather than here.
func isCurrencyDollar(src string, i int) bool {
	if i+1 >= len(src) {
		return true // trailing lone '$'
	}
	return src[i+1] == ' '
}

// scanDollarMath scans from the byte after an opening '$' for the matching
// unescaped closing '$' on the same run (no newline). It returns the inner
// body, the offset just past the close, and ok. An empty body ("$$" already
// handled elsewhere, but "$ $"-like spans) is rejected so plain prose with a
// stray pair is not swallowed.
//
// A '$' immediately followed by a digit does NOT close the span: it is
// currency ("$10"), so the scan keeps going. This is what keeps "$5 or $10"
// literal now that a digit right after the opening '$' is allowed (so a
// formula like "$1/\pi$" can render).
func scanDollarMath(src string, start int) (body string, end int, ok bool) {
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++ // skip the escaped rune
		case '\n':
			return "", 0, false // inline math never spans a newline
		case '$':
			if i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9' {
				continue // "$10" — currency, not a closing delimiter
			}
			if src[i-1] == ' ' {
				continue // "$a and $b" — a space before the close means the
				// second '$' opens a new token (a var/price), it does not
				// close the first. Keep scanning for a real close.
			}
			inner := src[start:i]
			if strings.TrimSpace(inner) == "" {
				return "", 0, false
			}
			return inner, i + 1, true
		}
	}
	return "", 0, false
}

// scanParenMath scans from the byte after an opening "\(" for the matching
// "\)". It returns the inner body, the offset just past the close, and ok.
func scanParenMath(src string, start int) (body string, end int, ok bool) {
	for i := start; i+1 < len(src)+1 && i < len(src); i++ {
		if src[i] == '\\' {
			if i+1 < len(src) && src[i+1] == ')' {
				return src[start:i], i + 2, true
			}
			i++ // skip any other escape
			continue
		}
		if src[i] == '\n' {
			return "", 0, false
		}
	}
	return "", 0, false
}

// IsInlineOpen reports whether an inline math delimiter opens at byte offset i
// in src (a "$" that is not currency/escaped/"$$", or a "\("). It is the
// cheap predicate the markdown inline scanner uses to decide whether to call
// FindInline; FindInline itself still validates the close.
func IsInlineOpen(src string, i int) bool {
	if i < 0 || i >= len(src) {
		return false
	}
	switch src[i] {
	case '$':
		if i+1 < len(src) && src[i+1] == '$' {
			return false
		}
		return !isCurrencyDollar(src, i)
	case '\\':
		return i+1 < len(src) && src[i+1] == '('
	}
	return false
}

// IsDisplayFence reports whether the whole (trimmed) line is a display-math
// fence on its own line — "$$", "\[", or "\]" — the shape the Phase 2 Write
// loop uses to open/close a display block. It intentionally does not match a
// "$$...$$" formula written on a single line (that is handled as a body, not a
// fence).
func IsDisplayFence(line string) bool {
	t := strings.TrimSpace(line)
	return t == "$$" || t == `\[` || t == `\]`
}

// DisplayOpen reports whether the trimmed line opens a display-math block:
// either the bare fence "$$" or "\[" on its own line (the multi-line form), or a
// complete one-line formula "$$…$$" / "\[…\]". It returns the inner body (empty
// for the bare-fence multi-line form) and whether the formula is complete on
// this single line (oneLine). The markdown Write loop uses it to decide between
// buffering until a closing fence and rendering a one-liner immediately.
//
// A bare "\]" or a lone "$$" closing an already-open block is NOT an opener; the
// caller tracks open state separately (a "$$" is ambiguous open/close, resolved
// by that state). DisplayOpen only classifies a line as a potential OPENER.
func DisplayOpen(line string) (body string, oneLine, ok bool) {
	t := strings.TrimSpace(line)
	// One-line dollar form: "$$ … $$" with a non-empty middle.
	if strings.HasPrefix(t, "$$") && strings.HasSuffix(t, "$$") && len(t) > 4 {
		inner := strings.TrimSpace(t[2 : len(t)-2])
		if inner != "" {
			return inner, true, true
		}
	}
	// One-line bracket form: "\[ … \]".
	if strings.HasPrefix(t, `\[`) && strings.HasSuffix(t, `\]`) && len(t) > 4 {
		inner := strings.TrimSpace(t[2 : len(t)-2])
		if inner != "" {
			return inner, true, true
		}
	}
	// Multi-line openers: a bare "$$" or "\[" on its own line.
	if t == "$$" || t == `\[` {
		return "", false, true
	}
	return "", false, false
}

// IsDisplayClose reports whether the trimmed line closes an open display-math
// block: a bare "$$" or "\]" on its own line.
func IsDisplayClose(line string) bool {
	t := strings.TrimSpace(line)
	return t == "$$" || t == `\]`
}

// StripDelimiters removes a surrounding math delimiter pair from body when
// present, returning the inner text. It handles $$...$$, $...$, \[...\], and
// \(...\); text without a recognized pair is returned unchanged (trimmed).
// This is the tolerant entry point ApproxInline uses when a caller forgets to
// strip the delimiters first.
func StripDelimiters(body string) string {
	s := strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(s, "$$") && strings.HasSuffix(s, "$$") && len(s) >= 4:
		return strings.TrimSpace(s[2 : len(s)-2])
	case strings.HasPrefix(s, `\[`) && strings.HasSuffix(s, `\]`) && len(s) >= 4:
		return strings.TrimSpace(s[2 : len(s)-2])
	case strings.HasPrefix(s, `\(`) && strings.HasSuffix(s, `\)`) && len(s) >= 4:
		return strings.TrimSpace(s[2 : len(s)-2])
	case strings.HasPrefix(s, "$") && strings.HasSuffix(s, "$") && len(s) >= 2:
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// CleanSource returns a readable linear fallback for a display formula that the
// 2D engine could not lay out (an unsupported Tier-3 construct, a malformed
// group). Rather than dump raw TeX, it runs the same single-line approximation
// the inline path uses (ApproxInline): greek/operators map to their Unicode
// glyph, \frac{a}{b} becomes "a/b", \sqrt{x} becomes "√(x)", scripts use
// super/subscript runes where they exist, \text{…}/\mathbf{…}/\mathcal{…} unwrap
// to their inner text, layout/size/spacing macros drop, and an unknown macro
// keeps its name without the backslash. The goal is that even an unrenderable
// formula shows readable "√(b²-4ac)"-style text, never "\sqrt{b^2-4ac}".
//
// It delegates to ApproxInline so the display fallback and inline math share one
// linear renderer; ApproxInline already strips a surrounding delimiter pair,
// guarantees a single physical line, and never emits a combining mark. Pure and
// deterministic — the graceful degradation path.
func CleanSource(latex string) string {
	return ApproxInline(latex)
}
