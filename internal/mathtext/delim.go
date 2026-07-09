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
// after it is a digit or ASCII space, or when no unescaped closing "$" follows
// on the same run — the classic "it costs $5 to $10" case. "$$" is a display
// fence and is not matched here.
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

// isCurrencyDollar reports whether the '$' at byte offset i is currency rather
// than a math opener: it is followed by a digit or a space, which no real
// inline formula starts with.
func isCurrencyDollar(src string, i int) bool {
	if i+1 >= len(src) {
		return true // trailing lone '$'
	}
	c := src[i+1]
	return c == ' ' || (c >= '0' && c <= '9')
}

// scanDollarMath scans from the byte after an opening '$' for the matching
// unescaped closing '$' on the same run (no newline). It returns the inner
// body, the offset just past the close, and ok. An empty body ("$$" already
// handled elsewhere, but "$ $"-like spans) is rejected so plain prose with a
// stray pair is not swallowed.
func scanDollarMath(src string, start int) (body string, end int, ok bool) {
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++ // skip the escaped rune
		case '\n':
			return "", 0, false // inline math never spans a newline
		case '$':
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

// CleanSource returns a readable linear fallback for a LaTeX fragment that the
// approximation cannot improve: it strips any surrounding delimiter pair and
// collapses runs of whitespace to single spaces, leaving the LaTeX otherwise
// intact. It is pure and deterministic — the graceful degradation path.
func CleanSource(latex string) string {
	return collapseSpaces(StripDelimiters(latex))
}

// collapseSpaces trims s and replaces every run of ASCII/Unicode whitespace
// with a single space, producing a single-line string.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	started := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			space = true
			continue
		}
		if space && started {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		space = false
		started = true
	}
	return b.String()
}
