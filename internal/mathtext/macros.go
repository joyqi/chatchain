package mathtext

// Macro classification tables for the Phase 2 parser: which macro names are
// big operators, word operators, named functions, layout/spacing/style noise,
// and which \begin environments map to a Matrix. Kept separate from parse.go so
// the parser reads as pure control flow and the data lives in one place. The
// membership sets are seeded from the same Tier-1 subset the spec defines
// (docs/design/math-rendering.md) and reuse the greek/operator tables in
// symbols.go for the actual glyphs.

// bigOpKind maps a glyph big-operator macro name to the canonical BigOp.Op
// value the layout engine switches on. \iint/\iiint/\oint collapse to "int",
// the \bigX operators collapse to their family so the engine draws the base
// glyph ("sum" for \bigcup-like, "prod" for \bigotimes-like) — the exact glyph
// is chosen at layout time; here we only need the limit-stacking behaviour,
// which is identical within a family.
func bigOpKind(name string) string {
	switch name {
	case "prod", "coprod", "bigotimes", "bigodot", "bigwedge", "bigvee":
		return "prod"
	case "int", "iint", "iiint", "oint":
		return "int"
	default:
		// sum, bigcup, bigcap, bigoplus, …
		return "sum"
	}
}

// bigOpSingleGlyph returns the exact single glyph a big operator draws with.
// bigOpKind collapses operators into "sum"/"prod"/"int" FAMILIES for the tall
// shape and limit placement, but the drawn glyph must stay the operator's own
// (a union is not a summation), so the specific glyph is chosen here. All are
// standard N-ary math operators of uniseg width 1 (no combining marks).
func bigOpSingleGlyph(name string) string {
	switch name {
	case "sum":
		return "\u2211" // ∑
	case "prod":
		return "\u220f" // ∏
	case "coprod":
		return "\u2210" // ∐
	case "int":
		return "\u222b" // ∫
	case "iint":
		return "\u222c" // ∬
	case "iiint":
		return "\u222d" // ∭
	case "oint":
		return "\u222e" // ∮
	case "bigcup":
		return "\u22c3" // ⋃
	case "bigcap":
		return "\u22c2" // ⋂
	case "bigoplus":
		return "\u2a01" // ⨁
	case "bigotimes":
		return "\u2a02" // ⨂
	case "bigodot":
		return "\u2a00" // ⨀
	case "bigwedge":
		return "\u22c0" // ⋀
	case "bigvee":
		return "\u22c1" // ⋁
	default:
		return "\u2211" // ∑ (should not happen; parser only passes known ops)
	}
}

// wordOpName returns the upright name a word operator draws with. Most match
// their macro name; a couple use a conventional spelling.
func wordOpName(name string) string {
	switch name {
	case "limsup":
		return "lim sup"
	case "liminf":
		return "lim inf"
	default:
		return name
	}
}

// funcNames is the set of upright named functions that render as their name
// (\sin -> "sin") and take no limits. \lim and friends are handled as word
// BigOps (they take under-limits), so they are NOT in this set.
var funcNames = map[string]bool{
	"sin": true, "cos": true, "tan": true, "cot": true, "sec": true, "csc": true,
	"sinh": true, "cosh": true, "tanh": true, "coth": true,
	"arcsin": true, "arccos": true, "arctan": true,
	"log": true, "ln": true, "lg": true, "exp": true,
	"mod": true, "bmod": true, "pmod": true,
}

// isFuncName reports whether name is an upright named function.
func isFuncName(name string) bool { return funcNames[name] }

// accentKinds maps a single-argument accent macro to its AccentKind. The accent
// is drawn as a glyph ROW ABOVE the base (like the sqrt vinculum / SymPy's
// pretty printer), never as a combining mark. \overline reuses the full-width
// drawn bar (AccentBar) so \overline{ab} covers the whole width.
var accentKinds = map[string]AccentKind{
	"hat":       AccentHat,
	"widehat":   AccentHat,
	"bar":       AccentBar,
	"overline":  AccentBar,
	"vec":       AccentVec,
	"tilde":     AccentTilde,
	"widetilde": AccentTilde,
	"dot":       AccentDot,
	"ddot":      AccentDdot,
}

// accentKind reports the AccentKind for a single-arg accent macro, and whether
// name is such a macro.
func accentKind(name string) (AccentKind, bool) {
	k, ok := accentKinds[name]
	return k, ok
}

// mathFontStyles maps a math-font macro to the MathStyle its single group
// argument renders in. mathbb/mathcal/mathfrak have Unicode math-alphanumeric
// variants and are mapped per letter; the plain styles (mathrm/mathbf/mathsf/
// mathit/boldsymbol) degrade every letter to itself (no SGR bold inside the
// glyph layout — the box model stays glyph-plain).
var mathFontStyles = map[string]MathStyle{
	"mathbb":     StyleBB,
	"mathcal":    StyleCal,
	"mathscr":    StyleCal,
	"mathfrak":   StyleFrak,
	"mathbf":     StylePlain,
	"boldsymbol": StylePlain,
	"bm":         StylePlain,
	"mathrm":     StylePlain,
	"mathit":     StylePlain,
	"mathsf":     StylePlain,
	"mathtt":     StylePlain,
	"mathnormal": StylePlain,
}

// mathFontStyle reports the MathStyle for a math-font macro, and whether name is
// one. A math-font macro takes a single group argument whose letters are mapped
// (or degrade to themselves), so it never causes a fallback.
func mathFontStyle(name string) (MathStyle, bool) {
	s, ok := mathFontStyles[name]
	return s, ok
}

// layoutMacroSet lists spacing/style/size macros that carry no glyph and take
// no structural argument in the Tier-1 subset: the parser drops them and keeps
// scanning. \left/\right and \begin/\end are NOT here — they are structural and
// handled explicitly in parseCommand.
var layoutMacroSet = map[string]bool{
	// horizontal spacing
	"quad": true, "qquad": true, "!": true, ",": true, ":": true, ";": true,
	" ": true, "thinspace": true, "medspace": true, "thickspace": true,
	"negthinspace": true, "negmedspace": true, "negthickspace": true,
	// math style / size selectors
	"displaystyle": true, "textstyle": true, "scriptstyle": true,
	"scriptscriptstyle": true, "limits": true, "nolimits": true,
	"big": true, "Big": true, "bigg": true, "Bigg": true,
	"bigl": true, "Bigl": true, "biggl": true, "Biggl": true,
	"bigr": true, "Bigr": true, "biggr": true, "Biggr": true,
	"bigm": true, "Bigm": true, "biggm": true, "Biggm": true,
	// numbering / tagging noise from aligned/align bodies (no glyph, no argument).
	"nonumber": true, "notag": true,
}

// isLayoutMacro reports whether name is a droppable layout/spacing/style macro.
func isLayoutMacro(name string) bool { return layoutMacroSet[name] }

// matrixEnvs is the set of \begin environments the parser turns into a Matrix.
// The array/matrix family plus cases; align/gather/eqnarray are deliberately
// absent (Tier-3) so they trigger the fallback.
var matrixEnvs = map[string]bool{
	"matrix":      true,
	"pmatrix":     true,
	"bmatrix":     true,
	"Bmatrix":     true,
	"vmatrix":     true,
	"Vmatrix":     true,
	"smallmatrix": true,
	"cases":       true,
}

// isMatrixEnv reports whether env is a supported matrix/cases environment.
func isMatrixEnv(env string) bool { return matrixEnvs[env] }

// alignedEnvs is the set of alignment environments parsed into a Matrix with no
// surrounding delimiters: rows split on \\, columns on &. "aligned"/"align"/
// "split" line their equations up on the & column; "gathered"/"gather" are a
// single centered column. All are parsed by the same matrix machinery; only the
// layout (matrixDelimsFor / alignment) differs by Env.
var alignedEnvs = map[string]bool{
	"aligned":  true,
	"align":    true,
	"split":    true,
	"gathered": true,
	"gather":   true,
	"eqnarray": true,
}

// isAlignedEnv reports whether env is an alignment environment (no brackets).
func isAlignedEnv(env string) bool { return alignedEnvs[env] }

// isGatheredEnv reports whether env centers a single column (gather/gathered).
func isGatheredEnv(env string) bool {
	return env == "gathered" || env == "gather"
}
