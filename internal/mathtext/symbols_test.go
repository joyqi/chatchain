package mathtext

import "testing"

func TestSymbolRune(t *testing.T) {
	cases := []struct {
		name string
		want rune
	}{
		{"alpha", 'α'},
		{"Alpha", 0}, // no LaTeX \Alpha macro; must NOT resolve
		{"beta", 'β'},
		{"pi", 'π'},
		{"Pi", 'Π'},
		{"Sigma", 'Σ'},
		{"Omega", 'Ω'},
		{"phi", 'φ'},
		{"varphi", 'ϕ'},
		{"infty", '∞'},
		{"times", '×'},
		{"leq", '≤'},
		{"le", '≤'},
		{"geq", '≥'},
		{"neq", '≠'},
		{"approx", '≈'},
		{"equiv", '≡'},
		{"partial", '∂'},
		{"nabla", '∇'},
		{"sum", '∑'},
		{"prod", '∏'},
		{"int", '∫'},
		{"in", '∈'},
		{"subset", '⊂'},
		{"cup", '∪'},
		{"cap", '∩'},
		{"forall", '∀'},
		{"exists", '∃'},
		{"rightarrow", '→'},
		{"to", '→'},
		{"Rightarrow", '⇒'},
		{"ldots", '…'},
		{"dots", '…'},
		{"cdot", '⋅'},
		{"pm", '±'},
		{"div", '÷'},
	}
	for _, c := range cases {
		got, ok := symbolRune(c.name)
		if c.want == 0 {
			if ok {
				t.Errorf("symbolRune(%q) = %q, want not found", c.name, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("symbolRune(%q) = %q (ok=%v), want %q", c.name, got, ok, c.want)
		}
	}
}

func TestGreekAliasesDistinct(t *testing.T) {
	// Uppercase Greek must map to the uppercase glyph, distinct from lowercase.
	lower, _ := symbolRune("sigma")
	upper, _ := symbolRune("Sigma")
	if lower == upper {
		t.Errorf("sigma and Sigma both map to %q; expected distinct glyphs", lower)
	}
}

func TestSuperscriptAvailability(t *testing.T) {
	// Present: every digit, the five symbols, and the encoded latin set.
	present := "0123456789+-=()abcdefghijklmnoprstuvwxyz"
	for _, r := range present {
		if _, ok := superscriptRune(r); !ok {
			t.Errorf("superscriptRune(%q) = not found, want a glyph", r)
		}
	}
	// Known-missing: 'q' has no superscript; capitals have none.
	for _, r := range []rune{'q', 'B', 'Q', 'Z', '/', '<'} {
		if g, ok := superscriptRune(r); ok {
			t.Errorf("superscriptRune(%q) = %q, want not found (linear fallback)", r, g)
		}
	}
}

func TestSubscriptAvailability(t *testing.T) {
	// Present: every digit, the five symbols, and the SMALLER latin subscript set.
	present := "0123456789+-=()aehijklmnoprstuvx"
	for _, r := range present {
		if _, ok := subscriptRune(r); !ok {
			t.Errorf("subscriptRune(%q) = not found, want a glyph", r)
		}
	}
	// Known-missing subscripts: b c d f g q w y z and all capitals fall back to
	// linear text (never a combining mark).
	for _, r := range []rune{'b', 'c', 'd', 'f', 'g', 'q', 'w', 'y', 'z', 'B', 'X'} {
		if g, ok := subscriptRune(r); ok {
			t.Errorf("subscriptRune(%q) = %q, want not found (linear fallback)", r, g)
		}
	}
}

func TestSuperscriptGlyphValues(t *testing.T) {
	// Spot-check exact code points against verified Unicode data.
	cases := map[rune]rune{
		'2': '²', // U+00B2 legacy
		'0': '⁰', // U+2070
		'n': 'ⁿ', // U+207F
		'i': 'ⁱ', // U+2071
		'x': 'ˣ', // U+02E3
	}
	for base, want := range cases {
		got, ok := superscriptRune(base)
		if !ok || got != want {
			t.Errorf("superscriptRune(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestSubscriptGlyphValues(t *testing.T) {
	cases := map[rune]rune{
		'0': '₀', // U+2080
		'i': 'ᵢ', // U+1D62
		'j': 'ⱼ', // U+2C7C
		'x': 'ₓ', // U+2093
		'+': '₊', // U+208A
	}
	for base, want := range cases {
		got, ok := subscriptRune(base)
		if !ok || got != want {
			t.Errorf("subscriptRune(%q) = %q, want %q", base, got, want)
		}
	}
}
