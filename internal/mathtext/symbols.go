// Package mathtext renders LaTeX math emitted by models into terminal-friendly
// text. Phase 1 covers a single-line Unicode approximation of inline math
// (greek/operators/relations, super/subscripts where real glyphs exist) with a
// graceful linear fallback; the 2D display-block engine is Phase 2.
//
// The symbol tables below are seeded from the go-latex/latex project's
// internal/tex2unicode macro-name -> rune mapping (BSD-3-Clause, archived). No
// go-latex code is imported: only the mapping VALUES this package needs were
// hand-transcribed, and every code point was verified against the Unicode
// Character Database. Attribution and the upstream license are preserved for
// the borrowed data.
package mathtext

// greekSymbols maps a LaTeX greek macro name (without the leading backslash) to
// its Unicode letter. Both the lowercase and the distinct uppercase forms are
// included; the "var" variants map to their Unicode variant glyphs. Seed:
// go-latex/latex internal/tex2unicode (BSD-3-Clause).
var greekSymbols = map[string]rune{
	"alpha":      'α', // U+03B1
	"beta":       'β', // U+03B2
	"gamma":      'γ', // U+03B3
	"Gamma":      'Γ', // U+0393
	"delta":      'δ', // U+03B4
	"Delta":      'Δ', // U+0394
	"epsilon":    'ε', // U+03B5
	"varepsilon": 'ε', // U+03B5
	"zeta":       'ζ', // U+03B6
	"eta":        'η', // U+03B7
	"theta":      'θ', // U+03B8
	"Theta":      'Θ', // U+0398
	"vartheta":   'ϑ', // U+03D1
	"iota":       'ι', // U+03B9
	"kappa":      'κ', // U+03BA
	"lambda":     'λ', // U+03BB
	"Lambda":     'Λ', // U+039B
	"mu":         'μ', // U+03BC
	"nu":         'ν', // U+03BD
	"xi":         'ξ', // U+03BE
	"Xi":         'Ξ', // U+039E
	"pi":         'π', // U+03C0
	"Pi":         'Π', // U+03A0
	"varpi":      'ϖ', // U+03D6
	"rho":        'ρ', // U+03C1
	"varrho":     'ϱ', // U+03F1
	"sigma":      'σ', // U+03C3
	"Sigma":      'Σ', // U+03A3
	"varsigma":   'ς', // U+03C2
	"tau":        'τ', // U+03C4
	"upsilon":    'υ', // U+03C5
	"Upsilon":    'Υ', // U+03A5
	"phi":        'φ', // U+03C6
	"varphi":     'ϕ', // U+03D5
	"Phi":        'Φ', // U+03A6
	"chi":        'χ', // U+03C7
	"psi":        'ψ', // U+03C8
	"Psi":        'Ψ', // U+03A8
	"omega":      'ω', // U+03C9
	"Omega":      'Ω', // U+03A9
}

// opSymbols maps a LaTeX operator/relation/miscellaneous macro name (without
// the leading backslash) to its Unicode glyph. Common aliases (le/leq, to/
// rightarrow, dots/ldots) share a target. Seed: go-latex/latex
// internal/tex2unicode (BSD-3-Clause).
var opSymbols = map[string]rune{
	// binary operators
	"times":    '×', // U+00D7
	"div":      '÷', // U+00F7
	"pm":       '±', // U+00B1
	"mp":       '∓', // U+2213
	"cdot":     '⋅', // U+22C5
	"ast":      '∗', // U+2217
	"star":     '⋆', // U+22C6
	"circ":     '∘', // U+2218
	"bullet":   '∙', // U+2219
	"oplus":    '⊕', // U+2295
	"otimes":   '⊗', // U+2297
	"odot":     '⊙', // U+2299
	"setminus": '∖', // U+2216

	// relations
	"leq":    '≤', // U+2264
	"le":     '≤', // U+2264
	"geq":    '≥', // U+2265
	"ge":     '≥', // U+2265
	"neq":    '≠', // U+2260
	"ne":     '≠', // U+2260
	"approx": '≈', // U+2248
	"equiv":  '≡', // U+2261
	"sim":    '∼', // U+223C
	"simeq":  '≃', // U+2243
	"cong":   '≅', // U+2245
	"propto": '∝', // U+221D
	"ll":     '≪', // U+226A
	"gg":     '≫', // U+226B
	"perp":   '⊥', // U+22A5
	"angle":  '∠', // U+2220
	"mid":    '∣', // U+2223 (divides / "such that" bar)
	"nmid":   '∤', // U+2224
	"asymp":  '≍', // U+224D
	"doteq":  '≐', // U+2250

	// set theory / logic
	"in":       '∈', // U+2208
	"notin":    '∉', // U+2209
	"ni":       '∋', // U+220B
	"subset":   '⊂', // U+2282
	"subseteq": '⊆', // U+2286
	"supset":   '⊃', // U+2283
	"supseteq": '⊇', // U+2287
	"cup":      '∪', // U+222A
	"cap":      '∩', // U+2229
	"emptyset": '∅', // U+2205
	"forall":   '∀', // U+2200
	"exists":   '∃', // U+2203
	"nexists":  '∄', // U+2204
	"neg":      '¬', // U+00AC
	"lnot":     '¬', // U+00AC
	"land":     '∧', // U+2227
	"wedge":    '∧', // U+2227
	"lor":      '∨', // U+2228
	"vee":      '∨', // U+2228
	"parallel": '∥', // U+2225

	// big operators / calculus
	"partial": '∂', // U+2202
	"nabla":   '∇', // U+2207
	"sum":     '∑', // U+2211
	"prod":    '∏', // U+220F
	"int":     '∫', // U+222B
	"oint":    '∮', // U+222E
	"infty":   '∞', // U+221E
	"surd":    '√', // U+221A

	// arrows
	"rightarrow":     '→', // U+2192
	"to":             '→', // U+2192
	"leftarrow":      '←', // U+2190
	"gets":           '←', // U+2190
	"leftrightarrow": '↔', // U+2194
	"Rightarrow":     '⇒', // U+21D2
	"Leftarrow":      '⇐', // U+21D0
	"Leftrightarrow": '⇔', // U+21D4
	"mapsto":         '↦', // U+21A6

	// dots
	"ldots": '…', // U+2026
	"dots":  '…', // U+2026
	"cdots": '⋯', // U+22EF
	"vdots": '⋮', // U+22EE
	"ddots": '⋱', // U+22F1

	// bare delimiter macros: outside \left…\right these are ordinary glyphs.
	// Without them a lone \langle would leak its raw macro name into the linear
	// fallback (the auto-sizing pair is handled separately in the parser).
	"langle":    '⟨',  // U+27E8
	"rangle":    '⟩',  // U+27E9
	"lfloor":    '⌊',  // U+230A
	"rfloor":    '⌋',  // U+230B
	"lceil":     '⌈',  // U+2308
	"rceil":     '⌉',  // U+2309
	"backslash": '\\', // U+005C

	// misc named symbols
	"prime": '′', // U+2032
	"aleph": 'ℵ', // U+2135
	"hbar":  'ℏ', // U+210F
	"ell":   'ℓ', // U+2113
	"Re":    'ℜ', // U+211C
	"Im":    'ℑ', // U+2111
	"wp":    '℘', // U+2118
}

// symbolRune looks up a macro name (no backslash) in the greek and operator
// tables, returning the Unicode glyph and whether it was found.
func symbolRune(name string) (rune, bool) {
	if r, ok := greekSymbols[name]; ok {
		return r, true
	}
	if r, ok := opSymbols[name]; ok {
		return r, true
	}
	return 0, false
}

// superscriptTable maps a base rune to its Unicode superscript glyph. It covers
// the ASCII digits, the symbols + - = ( ), and the Latin small letters that
// have a real superscript/modifier-letter code point. 'q' is deliberately
// absent: no superscript form exists for it. Every value was verified against
// the Unicode Character Database (legacy U+00B2/B3/B9, the Superscripts and
// Subscripts block U+2070-U+207F, and the modifier-letter blocks).
var superscriptTable = map[rune]rune{
	'0': '⁰', // U+2070
	'1': '¹', // U+00B9
	'2': '²', // U+00B2
	'3': '³', // U+00B3
	'4': '⁴', // U+2074
	'5': '⁵', // U+2075
	'6': '⁶', // U+2076
	'7': '⁷', // U+2077
	'8': '⁸', // U+2078
	'9': '⁹', // U+2079
	'+': '⁺', // U+207A
	'-': '⁻', // U+207B
	'=': '⁼', // U+207C
	'(': '⁽', // U+207D
	')': '⁾', // U+207E

	'a': 'ᵃ', // U+1D43
	'b': 'ᵇ', // U+1D47
	'c': 'ᶜ', // U+1D9C
	'd': 'ᵈ', // U+1D48
	'e': 'ᵉ', // U+1D49
	'f': 'ᶠ', // U+1DA0
	'g': 'ᵍ', // U+1D4D
	'h': 'ʰ', // U+02B0
	'i': 'ⁱ', // U+2071
	'j': 'ʲ', // U+02B2
	'k': 'ᵏ', // U+1D4F
	'l': 'ˡ', // U+02E1
	'm': 'ᵐ', // U+1D50
	'n': 'ⁿ', // U+207F
	'o': 'ᵒ', // U+1D52
	'p': 'ᵖ', // U+1D56
	'r': 'ʳ', // U+02B3
	's': 'ˢ', // U+02E2
	't': 'ᵗ', // U+1D57
	'u': 'ᵘ', // U+1D58
	'v': 'ᵛ', // U+1D5B
	'w': 'ʷ', // U+02B7
	'x': 'ˣ', // U+02E3
	'y': 'ʸ', // U+02B8
	'z': 'ᶻ', // U+1DBB
}

// subscriptTable maps a base rune to its Unicode subscript glyph. Subscripts
// cover far fewer letters than superscripts: only the ASCII digits, + - = ( ),
// and the Latin small letters a e h i j k l m n o p r s t u v x. Letters with
// no subscript code point (b c d f g q w y z and all capitals) are absent, so
// the caller falls back to LINEAR text (never a combining mark). Every value
// was verified against the Unicode Character Database (Subscripts block
// U+2080-U+208E, U+2090-U+209C, plus U+1D62-U+1D65 and U+2C7C).
var subscriptTable = map[rune]rune{
	'0': '₀', // U+2080
	'1': '₁', // U+2081
	'2': '₂', // U+2082
	'3': '₃', // U+2083
	'4': '₄', // U+2084
	'5': '₅', // U+2085
	'6': '₆', // U+2086
	'7': '₇', // U+2087
	'8': '₈', // U+2088
	'9': '₉', // U+2089
	'+': '₊', // U+208A
	'-': '₋', // U+208B
	'=': '₌', // U+208C
	'(': '₍', // U+208D
	')': '₎', // U+208E

	'a': 'ₐ', // U+2090
	'e': 'ₑ', // U+2091
	'h': 'ₕ', // U+2095
	'i': 'ᵢ', // U+1D62
	'j': 'ⱼ', // U+2C7C
	'k': 'ₖ', // U+2096
	'l': 'ₗ', // U+2097
	'm': 'ₘ', // U+2098
	'n': 'ₙ', // U+2099
	'o': 'ₒ', // U+2092
	'p': 'ₚ', // U+209A
	'r': 'ᵣ', // U+1D63
	's': 'ₛ', // U+209B
	't': 'ₜ', // U+209C
	'u': 'ᵤ', // U+1D64
	'v': 'ᵥ', // U+1D65
	'x': 'ₓ', // U+2093
}

// superscriptRune returns the Unicode superscript glyph for a base rune when a
// real code point exists, else ok=false so the caller keeps the base linear.
func superscriptRune(r rune) (rune, bool) {
	g, ok := superscriptTable[r]
	return g, ok
}

// subscriptRune returns the Unicode subscript glyph for a base rune when a real
// code point exists, else ok=false so the caller keeps the base linear. The
// subscript set is much smaller than the superscript set (see subscriptTable).
func subscriptRune(r rune) (rune, bool) {
	g, ok := subscriptTable[r]
	return g, ok
}

// AccentKind selects the glyph an Accent node draws in a row ABOVE its base.
// Every kind is a normal spacing glyph (uniseg width 1) — NEVER a combining
// mark (U+0300–U+036F): the accent is a drawn row, like the sqrt vinculum.
type AccentKind int

const (
	AccentHat   AccentKind = iota // \hat   -> '^'  (U+005E, ASCII circumflex)
	AccentBar                     // \bar/\overline -> drawn '─' (U+2500) full width
	AccentVec                     // \vec   -> '→'  (U+2192 rightwards arrow)
	AccentTilde                   // \tilde -> '~'  (U+007E ASCII tilde)
	AccentDot                     // \dot   -> '·'  (U+00B7 middle dot)
	AccentDdot                    // \ddot  -> '··' (two U+00B7 middle dots)
)

// accentGlyph returns the drawn accent glyph for a kind and whether it spans the
// FULL width of the base (a bar) or is a single centered mark. Every glyph is a
// spacing character verified against Unicode; none is in U+0300–U+036F. The
// AccentBar case returns "" because its glyph is the full-width drawn rule (the
// layout uses AccentBar's row-fill, not a single glyph).
func accentGlyph(k AccentKind) (glyph string, fullWidth bool) {
	switch k {
	case AccentHat:
		return "^", false // U+005E
	case AccentBar:
		return "", true // full-width drawn ─ (U+2500)
	case AccentVec:
		return "→", false // →
	case AccentTilde:
		return "~", false // U+007E
	case AccentDot:
		return "·", false // ·
	case AccentDdot:
		return "··", false // ··
	default:
		return "^", false
	}
}

// MathStyle selects a Unicode math-alphanumeric variant for a math-font macro
// (\mathbb, \mathcal, \mathfrak) or the plain style (letters degrade to
// themselves) for the upright/bold/sans/italic fonts, which the glyph-plain box
// layout renders without SGR.
type MathStyle int

const (
	StylePlain MathStyle = iota // letters unchanged (mathrm/mathbf/mathsf/mathit)
	StyleBB                     // blackboard bold  (mathbb)  ℝ ℂ ℕ …
	StyleCal                    // calligraphic     (mathcal) 𝒜 …
	StyleFrak                   // fraktur          (mathfrak) 𝔄 …
)

// blackboardBold maps an ASCII letter to its blackboard-bold code point. Most
// letters live in the contiguous Mathematical Alphanumeric Symbols block
// (U+1D538 A … / U+1D552 a …), but a handful of uppercase letters were unified
// into the Letterlike Symbols block long before that block existed and have
// "holes" at the systematic position — those (C H N P Q R Z) use the legacy
// code point. Verified against the Unicode Character Database.
var blackboardBold = map[rune]rune{
	'C': 'ℂ', // ℂ
	'H': 'ℍ', // ℍ
	'N': 'ℕ', // ℕ
	'P': 'ℙ', // ℙ
	'Q': 'ℚ', // ℚ
	'R': 'ℝ', // ℝ
	'Z': 'ℤ', // ℤ
}

// calligraphicHoles / frakturHoles hold the Letterlike-Symbols exceptions where
// the systematic Mathematical Alphanumeric Symbols position is unassigned and a
// legacy code point must be used instead. Verified against the UCD.
var calligraphicHoles = map[rune]rune{
	'B': 'ℬ', // ℬ
	'E': 'ℰ', // ℰ
	'F': 'ℱ', // ℱ
	'H': 'ℋ', // ℋ
	'I': 'ℐ', // ℐ
	'L': 'ℒ', // ℒ
	'M': 'ℳ', // ℳ
	'R': 'ℛ', // ℛ
	'e': 'ℯ', // ℯ
	'g': 'ℊ', // ℊ
	'o': 'ℴ', // ℴ
}

var frakturHoles = map[rune]rune{
	'C': 'ℭ', // ℭ
	'H': 'ℌ', // ℌ
	'I': 'ℑ', // ℑ
	'R': 'ℜ', // ℜ
	'Z': 'ℨ', // ℨ
}

// mathFontRune maps an ASCII letter to its Unicode math-alphanumeric variant for
// the given style. For a style with no dedicated glyph (StylePlain) or an input
// that is not an ASCII letter, it returns r unchanged: an unmapped letter always
// degrades to itself, so a math-font macro never triggers a fallback. The
// systematic ranges are the Mathematical Alphanumeric Symbols block; the
// exception maps above cover the Letterlike-Symbols holes.
func mathFontRune(style MathStyle, r rune) rune {
	switch style {
	case StyleBB:
		if g, ok := blackboardBold[r]; ok {
			return g
		}
		if r >= 'A' && r <= 'Z' {
			return 0x1D538 + (r - 'A') // 𝔸 …
		}
		if r >= 'a' && r <= 'z' {
			return 0x1D552 + (r - 'a') // 𝕒 …
		}
		if r >= '0' && r <= '9' {
			return 0x1D7D8 + (r - '0') // 𝟘 …
		}
	case StyleCal:
		if g, ok := calligraphicHoles[r]; ok {
			return g
		}
		if r >= 'A' && r <= 'Z' {
			return 0x1D49C + (r - 'A') // 𝒜 …
		}
		if r >= 'a' && r <= 'z' {
			return 0x1D4B6 + (r - 'a') // 𝒶 …
		}
	case StyleFrak:
		if g, ok := frakturHoles[r]; ok {
			return g
		}
		if r >= 'A' && r <= 'Z' {
			return 0x1D504 + (r - 'A') // 𝔄 …
		}
		if r >= 'a' && r <= 'z' {
			return 0x1D51E + (r - 'a') // 𝔞 …
		}
	}
	return r
}

// applyMathFont maps every ASCII letter (and, for blackboard, digit) of s to its
// math-alphanumeric variant for the style, leaving any other rune untouched. The
// result stays a plain glyph string (no SGR), so the box layout draws it
// verbatim; an unmapped rune passes through so the call never fails.
func applyMathFont(style MathStyle, s string) string {
	if style == StylePlain {
		return s
	}
	var b []rune
	for _, r := range s {
		b = append(b, mathFontRune(style, r))
	}
	return string(b)
}
