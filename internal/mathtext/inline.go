package mathtext

import "strings"

// ApproxInline renders an inline math BODY to a single Unicode line. Callers
// normally pass a body with the delimiters already stripped, but ApproxInline
// is tolerant and strips a surrounding pair if present. It never returns a
// newline and never emits a combining mark: anything it cannot improve degrades
// to readable linear text (see CleanSource).
//
// Handled constructs:
//
//	\alpha, \times, \leq, …   -> Unicode symbol (greek/operator tables)
//	x^2, e^{-x}, a_i, x_{ij}  -> super/subscript runes when every char in the
//	                             script has a glyph, else linear "^2" / "_ij"
//	\frac{a}{b}               -> "a/b" (multi-token parts parenthesized)
//	\sqrt{x+1}                -> "√(x+1)"  (single atom: "√x")
//	\text{if }                -> the inner text verbatim
//	\left( \right) \, \; \quad \displaystyle \limits … -> stripped (layout only)
//	unknown \macro            -> the macro name without the backslash
func ApproxInline(latex string) string {
	body := StripDelimiters(latex)
	out := approx(body)
	// A single physical line is a hard guarantee: fold any stray newline, then
	// collapse the runs of spaces that macro-space handling can introduce (e.g.
	// \text{if } followed by a source space). Interior single spaces are kept.
	out = strings.ReplaceAll(out, "\n", " ")
	out = collapseSpaceRuns(out)
	return strings.TrimSpace(out)
}

// collapseSpaceRuns replaces every run of two or more ASCII spaces with a
// single space, leaving other characters untouched. It is narrower than
// collapseSpaces (which also strips tabs/newlines and trims): ApproxInline has
// already folded newlines, and only the double-space artifact needs squashing.
func collapseSpaceRuns(s string) string {
	if !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// layoutMacros are spacing/style macros that carry no glyph — they are dropped
// entirely (their argument, if any, is handled by the caller since these take
// none in the inline subset).
var layoutMacros = map[string]bool{
	"left": true, "right": true,
	"quad": true, "qquad": true,
	"displaystyle": true, "textstyle": true, "scriptstyle": true,
	"limits": true, "nolimits": true,
	"bigl": true, "bigr": true, "Bigl": true, "Bigr": true,
	"biggl": true, "biggr": true, "Biggl": true, "Biggr": true,
	"big": true, "Big": true, "bigg": true, "Bigg": true,
	"mathrm": true, "mathbf": true, "mathit": true, "mathsf": true,
	"boldsymbol": true, "operatorname": false, // operatorname handled specially
}

// approx is the recursive single-line approximator over a math fragment.
func approx(s string) string {
	r := []rune(s)
	var b strings.Builder
	i := 0
	for i < len(r) {
		c := r[i]
		switch {
		case c == '\\':
			i = emitMacro(&b, r, i)
		case c == '^' || c == '_':
			i = emitScript(&b, r, i)
		case c == '{' || c == '}':
			// Bare grouping braces carry no meaning once scripts/args are handled
			// by their own scanners; drop them so leftover groups read cleanly.
			i++
		case c == '~':
			// Non-breaking space in LaTeX.
			b.WriteByte(' ')
			i++
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String()
}

// emitMacro handles a backslash sequence starting at r[i] (r[i]=='\\') and
// returns the index just past what it consumed.
func emitMacro(b *strings.Builder, r []rune, i int) int {
	// Escaped punctuation: \$, \%, \&, \#, \_, \{, \}, and \\ -> the literal.
	if i+1 < len(r) {
		switch r[i+1] {
		case '$', '%', '&', '#', '_', '{', '}', ' ':
			b.WriteRune(r[i+1])
			return i + 2
		case '\\':
			// Line break in source: fold to a space for the single-line output.
			b.WriteByte(' ')
			return i + 2
		case ',', ';', ':', '!':
			// Thin/med/neg spaces: drop.
			return i + 2
		}
	}

	name, next, spaced := scanMacroName(r, i+1)
	if name == "" {
		// A lone backslash with nothing after it: drop it.
		return next
	}

	switch name {
	case "frac", "tfrac", "dfrac", "cfrac":
		return emitFrac(b, r, next)
	case "sqrt":
		return emitSqrt(b, r, next)
	case "text", "textrm", "textbf", "textit", "mathrm", "mathbf", "mathit",
		"mathsf", "mathtt", "boldsymbol", "operatorname":
		return emitTextArg(b, r, next)
	case "hat", "bar", "vec", "tilde", "dot", "ddot", "widehat", "widetilde",
		"overline", "underline":
		// Accents cannot be drawn inline without a combining mark (which is
		// forbidden), so P1 renders just the base argument, e.g. \vec{v} -> "v".
		// Faithful accent stacking is deferred to the 2D engine (P2/P3).
		return emitTextArg(b, r, next)
	}

	if layoutMacros[name] {
		// Layout-only macro: emit nothing, keep scanning after it. The swallowed
		// trailing space is intentionally dropped — these carry no glyph.
		return next
	}

	if sym, ok := symbolRune(name); ok {
		b.WriteRune(sym)
		// LaTeX swallows the space after a control word, but for a linear text
		// approximation that space is the only thing separating "α" from a
		// following "+" or "β"; restore a single space so "\alpha + \beta" reads
		// as "α + β" rather than "α+ β".
		if spaced {
			b.WriteByte(' ')
		}
		return next
	}

	// Unknown macro: best-effort is the bare name (e.g. \foo -> foo). This
	// reads better than keeping the backslash and rarely worse than dropping it.
	// A swallowed separating space is restored for the same readability reason.
	b.WriteString(name)
	if spaced {
		b.WriteByte(' ')
	}
	return next
}

// scanMacroName reads a macro name at r[i] (just after the backslash). LaTeX
// control words are runs of ASCII letters; a control symbol is a single
// non-letter. It returns the name, the index just past it (and any swallowed
// trailing spaces), and whether at least one trailing space was swallowed.
func scanMacroName(r []rune, i int) (string, int, bool) {
	if i >= len(r) {
		return "", i, false
	}
	if !isASCIILetter(r[i]) {
		// Control symbol (already-escaped punctuation is handled by the caller);
		// return the single rune as the "name". No trailing space is swallowed.
		return string(r[i]), i + 1, false
	}
	j := i
	for j < len(r) && isASCIILetter(r[j]) {
		j++
	}
	name := string(r[i:j])
	// LaTeX swallows the run of spaces after a control word.
	spaced := false
	for j < len(r) && r[j] == ' ' {
		j++
		spaced = true
	}
	return name, j, spaced
}

// emitScript handles a ^ or _ script starting at r[i]. It reads the script
// argument (a braced group or a single token) and, when every character in the
// approximated argument has a super/subscript glyph, emits those glyphs;
// otherwise it falls back to LINEAR text ("^2" / "_ij") so the result stays
// readable. Never emits a combining mark.
func emitScript(b *strings.Builder, r []rune, i int) int {
	kind := r[i] // '^' or '_'
	arg, next := scanScriptArg(r, i+1)
	// Approximate the argument first so nested macros (e.g. x^{\alpha}) resolve
	// to their glyph before the super/subscript mapping is attempted.
	inner := approx(arg)
	if mapped, ok := mapScript(inner, kind); ok {
		b.WriteString(mapped)
		return next
	}
	// Linear fallback: keep the marker so it stays readable, wrapping a
	// multi-rune script in braces would be noisier than useful — instead keep
	// the marker and the plain inner text.
	b.WriteRune(kind)
	b.WriteString(inner)
	return next
}

// mapScript maps every rune of s to its super- or subscript glyph, returning
// ok=false if ANY rune lacks one (so the caller uses the linear fallback).
func mapScript(s string, kind rune) (string, bool) {
	if s == "" {
		return "", false
	}
	var b strings.Builder
	for _, c := range s {
		var g rune
		var ok bool
		if kind == '^' {
			g, ok = superscriptRune(c)
		} else {
			g, ok = subscriptRune(c)
		}
		if !ok {
			return "", false
		}
		b.WriteRune(g)
	}
	return b.String(), true
}

// scanScriptArg reads a script's argument at r[i]: a braced group {…} (returned
// without the braces) or a single token (one rune, or a whole \macro). Returns
// the raw argument text and the index just past it.
func scanScriptArg(r []rune, i int) (string, int) {
	if i >= len(r) {
		return "", i
	}
	if r[i] == '{' {
		inner, next := scanGroup(r, i)
		return inner, next
	}
	if r[i] == '\\' {
		name, next, _ := scanMacroName(r, i+1)
		// Rebuild the macro token (without any swallowed trailing space) so the
		// script argument is exactly the macro, e.g. x^\alpha -> "\alpha".
		return `\` + name, next
	}
	return string(r[i]), i + 1
}

// emitFrac handles \frac{num}{den} at r[i] (just past the macro name). It
// renders "num/den", parenthesizing a part that is not a single atom so the
// slash binding stays unambiguous ("(a+b)/c").
func emitFrac(b *strings.Builder, r []rune, i int) int {
	num, i := readArg(r, i)
	den, i := readArg(r, i)
	b.WriteString(fracPart(num))
	b.WriteByte('/')
	b.WriteString(fracPart(den))
	return i
}

// fracPart approximates one side of a fraction and wraps it in parentheses
// when it is not a single visual atom (so "a+b" becomes "(a+b)" but "x" and
// "\alpha" stay bare).
func fracPart(arg string) string {
	s := approx(arg)
	if isSingleAtom(s) {
		return s
	}
	return "(" + s + ")"
}

// emitSqrt handles \sqrt{x} (and \sqrt[n]{x}) at r[i]. It renders "√(x)" for a
// compound radicand and "√x" for a single atom. An optional index [n] is
// rendered inline as a leading superscript-ish note is avoided; instead it is
// dropped for P1 (rare in inline text) but the radicand is still rendered.
func emitSqrt(b *strings.Builder, r []rune, i int) int {
	// Optional [n] index: skip it for the inline approximation.
	if i < len(r) && r[i] == '[' {
		_, i = scanBracket(r, i)
	}
	arg, i := readArg(r, i)
	s := approx(arg)
	b.WriteRune('√')
	if isSingleAtom(s) {
		b.WriteString(s)
	} else {
		b.WriteString("(" + s + ")")
	}
	return i
}

// emitTextArg handles \text{…} and font macros: it emits the inner content
// approximated (for \text this is verbatim prose; for math-font macros the
// inner is still math, so approx keeps symbols working).
func emitTextArg(b *strings.Builder, r []rune, i int) int {
	arg, i := readArg(r, i)
	b.WriteString(approx(arg))
	return i
}

// readArg reads the next macro argument at r[i]: a braced group (contents
// without braces) or a single token. Leading spaces are skipped. Returns the
// raw argument and the index just past it.
func readArg(r []rune, i int) (string, int) {
	for i < len(r) && r[i] == ' ' {
		i++
	}
	if i >= len(r) {
		return "", i
	}
	if r[i] == '{' {
		return scanGroup(r, i)
	}
	if r[i] == '\\' {
		name, next, _ := scanMacroName(r, i+1)
		return `\` + name, next
	}
	return string(r[i]), i + 1
}

// scanGroup reads a brace-balanced group starting at r[i] (r[i]=='{') and
// returns the inner contents (braces removed) and the index just past the
// matching close brace. An unbalanced group runs to end of input.
func scanGroup(r []rune, i int) (string, int) {
	depth := 0
	start := i + 1
	for j := i; j < len(r); j++ {
		switch r[j] {
		case '\\':
			j++ // skip escaped rune so \{ / \} do not affect depth
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(r[start:j]), j + 1
			}
		}
	}
	return string(r[start:]), len(r)
}

// scanBracket reads a [...] group starting at r[i] (r[i]=='[') and returns the
// inner contents and the index just past the ']'. Unbalanced runs to end.
func scanBracket(r []rune, i int) (string, int) {
	for j := i + 1; j < len(r); j++ {
		if r[j] == ']' {
			return string(r[i+1 : j]), j + 1
		}
	}
	return string(r[i+1:]), len(r)
}

// isSingleAtom reports whether s renders as a single visual atom for the
// purpose of fraction/root parenthesization: one rune, or a run with no
// operator/space that would make an unparenthesized slash ambiguous.
func isSingleAtom(s string) bool {
	rs := []rune(s)
	if len(rs) <= 1 {
		return true
	}
	for _, c := range rs {
		if strings.ContainsRune("+-*/=<> ", c) {
			return false
		}
	}
	// A short run of letters/digits (e.g. "xy", "12") still reads unambiguously
	// as a numerator, but keep it conservative: multi-rune numerators like "2x"
	// are fine bare because there is no operator. So treat operator-free runs as
	// atoms.
	return true
}

// isASCIILetter reports whether r is an ASCII a-z or A-Z.
func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
