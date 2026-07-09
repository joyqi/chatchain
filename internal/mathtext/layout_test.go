package mathtext

import (
	"strings"
	"testing"
)

// layoutOf parses in and lays it out, failing the test on a parse error.
func layoutOf(t *testing.T, in string) Box {
	t.Helper()
	n, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", in, err)
	}
	return layout(n)
}

// wantLayout asserts a laid-out formula's exact rows and baseline, and re-checks
// the box invariant (every row is the declared display width, uniseg-measured)
// plus the hard rule that no combining mark (U+0300–U+036F) ever appears.
func wantLayout(t *testing.T, in string, wantLines []string, wantBaseline int) {
	t.Helper()
	got := layoutOf(t, in)
	if got.Baseline != wantBaseline {
		t.Errorf("%q baseline = %d, want %d\n%s", in, got.Baseline, wantBaseline, got.String())
	}
	if len(got.Lines) != len(wantLines) {
		t.Fatalf("%q got %d rows, want %d:\n%s", in, len(got.Lines), len(wantLines), got.String())
	}
	for i := range wantLines {
		if got.Lines[i] != wantLines[i] {
			t.Errorf("%q row %d = %q, want %q\nfull:\n%s", in, i, got.Lines[i], wantLines[i], got.String())
		}
	}
	for i, l := range got.Lines {
		if w := boxWidth(l); w != got.Width {
			t.Errorf("%q row %d display width = %d, want %d (%q)", in, i, w, got.Width, l)
		}
	}
	if hasCombiningMark(got.String()) {
		t.Errorf("%q layout contains a combining mark:\n%s", in, got.String())
	}
}

// TestLayoutFracStacked: the canonical stacked fraction, numerator over a drawn
// bar over the denominator with the baseline on the bar.
func TestLayoutFracStacked(t *testing.T) {
	wantLayout(t, `\frac{a}{b}`, []string{
		"a",
		"─",
		"b",
	}, 1)
}

// TestLayoutFracWideDenominator: the bar spans the wider part and the narrow
// numerator centers over it.
func TestLayoutFracWideDenominator(t *testing.T) {
	wantLayout(t, `\frac{a+b}{c}`, []string{
		"a + b",
		"─────",
		"  c  ",
	}, 1)
}

// TestLayoutNestedFrac: a fraction whose numerator is itself a fraction stacks
// to five rows with the outer bar as the baseline.
func TestLayoutNestedFrac(t *testing.T) {
	wantLayout(t, `\frac{\frac{a}{b}}{c}`, []string{
		"a",
		"─",
		"b",
		"─",
		"c",
	}, 3)
}

// TestLayoutSqrtVinculum: a square root draws an underscore vinculum across the
// whole radicand with a ╲╱ check whose ╱ stroke rises to meet it — no combining
// overline, and the stroke is connected to the bar (not the old detached √+─).
func TestLayoutSqrtVinculum(t *testing.T) {
	wantLayout(t, `\sqrt{x+1}`, []string{
		"  ______",
		"╲╱ x + 1",
	}, 1)
}

// TestLayoutRootIndex: \sqrt[3]{x} puts the small index at the radical's upper
// left, in the crook above the ╲.
func TestLayoutRootIndex(t *testing.T) {
	wantLayout(t, `\sqrt[3]{x}`, []string{
		"3  __",
		" ╲╱ x",
	}, 1)
}

// TestLayoutSubSup: x_i^2 raises the exponent a row above the base and drops the
// subscript a row below, on the base's own column.
func TestLayoutSubSup(t *testing.T) {
	wantLayout(t, `x_i^2`, []string{
		" 2",
		"x ",
		" i",
	}, 1)
}

// TestLayoutSup: a plain superscript sits one row above the base.
func TestLayoutSup(t *testing.T) {
	wantLayout(t, `x^2`, []string{
		" 2",
		"x ",
	}, 1)
}

// TestLayoutSumWithLimits: ∑ with an upper limit above and a lower limit below,
// the operand to the right on the operator's baseline.
func TestLayoutSumWithLimits(t *testing.T) {
	wantLayout(t, `\sum_{i=1}^{n} i`, []string{
		"  n   ",
		"  ∑  i",
		"i = 1 ",
	}, 1)
}

// TestLayoutTallIntegral: an integral over a fraction grows to the drawn
// half-integral column (⌠ ⎮ ⌡) that brackets the integrand.
func TestLayoutTallIntegral(t *testing.T) {
	wantLayout(t, `\int \frac{1}{x}`, []string{
		"⌠1",
		"⎮─",
		"⌡x",
	}, 1)
}

// TestLayoutPmatrix: a 2x2 matrix wrapped in auto-sized parentheses.
func TestLayoutPmatrix(t *testing.T) {
	wantLayout(t, `\begin{pmatrix} a & b \\ c & d \end{pmatrix}`, []string{
		"⎛ a  b ⎞",
		"⎝ c  d ⎠",
	}, 0)
}

// TestLayoutBmatrix: the bracket variant uses the drawn square-bracket corners.
func TestLayoutBmatrix(t *testing.T) {
	wantLayout(t, `\begin{bmatrix} 1 & 2 \\ 3 & 4 \end{bmatrix}`, []string{
		"⎡ 1  2 ⎤",
		"⎣ 3  4 ⎦",
	}, 0)
}

// TestLayoutCases: a cases environment is left-aligned with a single left brace.
func TestLayoutCases(t *testing.T) {
	wantLayout(t, `\begin{cases} x & a \\ y & b \end{cases}`, []string{
		"⎧ x  a",
		"⎩ y  b",
	}, 0)
}

// TestLayoutDelimAroundFrac: \left( \frac{a}{b} \right) auto-sizes the parens to
// the fraction's height and aligns them on the fraction's bar baseline.
func TestLayoutDelimAroundFrac(t *testing.T) {
	wantLayout(t, `\left( \frac{a}{b} \right)`, []string{
		"⎛a⎞",
		"⎜─⎟",
		"⎝b⎠",
	}, 1)
}

// TestLayoutLim: \lim draws its upright word with the sub-limit below it.
func TestLayoutLim(t *testing.T) {
	wantLayout(t, `\lim_{x \to 0} f`, []string{
		" lim  f",
		"x → 0  ",
	}, 0)
}

// TestLayoutCJKText: \text{中文} keeps its wide runes and the box width is the
// uniseg display width (4 columns for two CJK glyphs), so it aligns in a grid.
func TestLayoutCJKText(t *testing.T) {
	got := layoutOf(t, `\text{中文}`)
	if got.Width != 4 {
		t.Errorf("CJK text width = %d, want 4\n%s", got.Width, got.String())
	}
	if got.String() != "中文" {
		t.Errorf("CJK text = %q, want 中文", got.String())
	}
}

// TestLayoutQuadraticFormula: the headline formula exercises fractions, ±,
// radicals with a vinculum, and a superscript all at once, and must be
// combining-mark-free.
func TestLayoutQuadraticFormula(t *testing.T) {
	got := layoutOf(t, `\frac{-b \pm \sqrt{b^2 - 4ac}}{2a}`)
	if hasCombiningMark(got.String()) {
		t.Errorf("quadratic formula has a combining mark:\n%s", got.String())
	}
	// The bar is the baseline and spans the full width.
	bar := got.Lines[got.Baseline]
	if strings.Trim(bar, "─") != "" || boxWidth(bar) != got.Width {
		t.Errorf("baseline row is not a full-width bar: %q", bar)
	}
	// It renders as five rows (index note, exponent, main line, bar, denominator).
	if got.Height() != 5 {
		t.Errorf("quadratic formula height = %d, want 5:\n%s", got.Height(), got.String())
	}
}

// TestLayoutAccentHat: \hat{f} draws a '^' row above the base f — a drawn glyph
// row, never a combining mark. The base keeps the baseline (shifted down one).
func TestLayoutAccentHat(t *testing.T) {
	wantLayout(t, `\hat{f}`, []string{
		"^",
		"f",
	}, 1)
}

// TestLayoutAccentVec: \vec{v} draws a '→' row above the base.
func TestLayoutAccentVec(t *testing.T) {
	wantLayout(t, `\vec{v}`, []string{
		"→",
		"v",
	}, 1)
}

// TestLayoutAccentDdot: \ddot{x} draws two middle dots above, widening the box
// so every row keeps the box-width invariant.
func TestLayoutAccentDdot(t *testing.T) {
	wantLayout(t, `\ddot{x}`, []string{
		"··",
		"x ",
	}, 1)
}

// TestLayoutOverline: \overline{ab} draws the full-width vinculum over the whole
// argument (reusing the sqrt bar), covering both letters.
func TestLayoutOverline(t *testing.T) {
	wantLayout(t, `\overline{ab}`, []string{
		"──",
		"ab",
	}, 1)
}

// TestLayoutMathbb: \mathbb{R} maps to the blackboard-bold ℝ (a single mapped
// atom), and \mathbf degrades to the plain letter.
func TestLayoutMathbb(t *testing.T) {
	if got := layoutOf(t, `\mathbb{R}`).String(); got != "ℝ" {
		t.Errorf("\\mathbb{R} = %q, want ℝ", got)
	}
	if got := layoutOf(t, `\mathbf{E}`).String(); got != "E" {
		t.Errorf("\\mathbf{E} = %q, want E (plain)", got)
	}
	if got := layoutOf(t, `\mathcal{L}`).String(); got != "ℒ" {
		t.Errorf("\\mathcal{L} = %q, want ℒ", got)
	}
}

// TestLayoutAligned: \begin{aligned} a &= b \\ c &= d \end{aligned} renders as
// two rows with the equations lined up on the relation, and NO brackets.
func TestLayoutAligned(t *testing.T) {
	got := layoutOf(t, `\begin{aligned} a &= b \\ c &= d \end{aligned}`)
	if got.Height() != 2 {
		t.Fatalf("aligned rendered %d rows, want 2:\n%s", got.Height(), got.String())
	}
	// No parenthesis/bracket glyphs wrap an alignment environment.
	if strings.ContainsAny(got.String(), "⎛⎝⎡⎣()[]") {
		t.Errorf("aligned should carry no delimiters:\n%s", got.String())
	}
	// Each row lines up on the '=' at the same column.
	c0 := strings.IndexRune(got.Lines[0], '=')
	c1 := strings.IndexRune(got.Lines[1], '=')
	if c0 < 0 || c0 != c1 {
		t.Errorf("aligned rows not lined up on '=': cols %d/%d\n%s", c0, c1, got.String())
	}
}

// TestLayoutLiteralBrace: \{ x \} renders literal curly braces around the body.
func TestLayoutLiteralBrace(t *testing.T) {
	got := layoutOf(t, `\{ x \}`).String()
	if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("\\{ x \\} = %q, want literal braces", got)
	}
}

// TestLayoutNoCombiningMarksBattery runs a spread of constructs through the
// layout and asserts none produce a combining mark — the hard rule of the whole
// engine.
func TestLayoutNoCombiningMarksBattery(t *testing.T) {
	for _, in := range []string{
		`\frac{a}{b}`,
		`\sqrt{x+1}`,
		`\sqrt[3]{x}`,
		`x_i^2`,
		`\sum_{i=1}^{n} i`,
		`\int_0^1 \frac{1}{x}`,
		`\begin{pmatrix} a & b \\ c & d \end{pmatrix}`,
		`\begin{cases} x & a \\ y & b \end{cases}`,
		`\left( \frac{a}{b} \right)`,
		`\lim_{x \to 0} f`,
		`\text{中文} + \alpha`,
		`\hat{f}`,
		`\vec{v}`,
		`\bar{x}`,
		`\dot{x}`,
		`\ddot{x}`,
		`\overline{ab}`,
		`\tilde{a}`,
		`\mathbb{R} \cup \mathbb{C}`,
		`\mathcal{L}`,
		`\begin{aligned} a &= b \\ c &= d \end{aligned}`,
	} {
		got := layoutOf(t, in)
		if hasCombiningMark(got.String()) {
			t.Errorf("%q produced a combining mark:\n%s", in, got.String())
		}
	}
}
