package mathtext

import (
	"strings"
	"testing"
)

func TestApproxInlineGolden(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Greek + operators, with restored separating spaces.
		{`\alpha + \beta`, "α + β"},
		{`5 \times 3`, "5 × 3"},
		{`\hbar\omega`, "ℏω"},

		// Superscripts: every char has a glyph -> unicode.
		{`x^2 + y^2`, "x² + y²"},
		{`E = mc^2`, "E = mc²"},
		{`e^{-x}`, "e⁻ˣ"},
		{`a^{10}`, "a¹⁰"},

		// Subscripts: available letters map, unavailable fall back to linear.
		{`a_i`, "aᵢ"},
		{`x_{ij}`, "xᵢⱼ"},  // both i and j have subscripts
		{`x_{10}`, "x₁₀"},  // digits have subscripts
		{`C_B`, "C_B"},     // capital B has NO subscript -> linear
		{`x_{ab}`, "x_ab"}, // 'b' has no subscript -> whole script linear

		// Fractions.
		{`\frac{a}{b}`, "a/b"},
		{`\frac{a+b}{c}`, "(a+b)/c"},

		// Roots.
		{`\sqrt{x}`, "√x"},
		{`\sqrt{x+1}`, "√(x+1)"},

		// Sum with limits (super+subscripts each fully mappable).
		{`\sum_{i=1}^{n} x_i`, "∑ᵢ₌₁ⁿ xᵢ"},

		// \text passthrough + relation with clean spacing.
		{`\text{if } x \geq 0`, "if x ≥ 0"},

		// Accent stripped (no combining mark in P1).
		{`\vec{v}`, "v"},

		// Layout-only macros dropped.
		{`\left( \frac{a}{b} \right)`, "( a/b )"},

		// Tolerant of delimiters left on by the caller.
		{`$x^2$`, "x²"},
		{`\(a + b\)`, "a + b"},
	}
	for _, c := range cases {
		got := ApproxInline(c.in)
		if got != c.want {
			t.Errorf("ApproxInline(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApproxInlineNeverMultiline(t *testing.T) {
	for _, in := range []string{"a\nb", `\frac{a}{b}`, "x^2 \\\\ y", "line1\nline2"} {
		if strings.Contains(ApproxInline(in), "\n") {
			t.Errorf("ApproxInline(%q) contains a newline", in)
		}
	}
}

func TestApproxInlineDollarNotMangled(t *testing.T) {
	// The escaped-dollar case that a caller might pass through: \$5 must become a
	// plain "$5", never treated as math markup by the approximator itself.
	if got := ApproxInline(`\$5 is cheap`); got != "$5 is cheap" {
		t.Errorf("ApproxInline(%q) = %q, want %q", `\$5 is cheap`, got, "$5 is cheap")
	}
}

func TestApproxInlineMissingSubscriptStaysLinear(t *testing.T) {
	// Every letter that lacks a subscript keeps the '_' marker (readable),
	// never a combining mark.
	for _, in := range []string{"C_B", "x_c", "y_d", "z_q", "n_w"} {
		got := ApproxInline(in)
		if !strings.Contains(got, "_") {
			t.Errorf("ApproxInline(%q) = %q, expected linear underscore fallback", in, got)
		}
		for _, r := range got {
			if r >= 0x0300 && r <= 0x036F {
				t.Errorf("ApproxInline(%q) = %q contains a combining mark", in, got)
			}
		}
	}
}

func TestApproxInlineNoCombiningMarks(t *testing.T) {
	// Broad guard: no output rune falls in a combining-mark block.
	inputs := []string{
		`x^2 + y_j`, `\vec{v}`, `\hat{x}`, `\sum_{i}^{n}`, `\theta^\circ`,
		`\frac{\alpha}{\beta}`, `C_B^A`,
	}
	for _, in := range inputs {
		for _, r := range ApproxInline(in) {
			if (r >= 0x0300 && r <= 0x036F) || (r >= 0x1AB0 && r <= 0x1AFF) ||
				(r >= 0x1DC0 && r <= 0x1DFF) || (r >= 0x20D0 && r <= 0x20FF) {
				t.Errorf("ApproxInline(%q) emitted combining mark U+%04X", in, r)
			}
		}
	}
}

func TestApproxInlineUnknownMacro(t *testing.T) {
	// An unknown macro degrades to its bare name (best effort), not the raw
	// backslash form.
	if got := ApproxInline(`\foobar x`); got != "foobar x" {
		t.Errorf("ApproxInline(%q) = %q, want %q", `\foobar x`, got, "foobar x")
	}
}
