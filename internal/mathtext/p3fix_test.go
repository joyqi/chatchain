package mathtext

import (
	"strings"
	"testing"
)

// These regression tests lock in the Phase-3 review fixes: the confident
// mis-renders and spacing defects an adversarial pass surfaced before P3 shipped.

// A bare delimiter macro (outside \left…\right) is an ordinary glyph, not a
// leaked macro name. Before the fix `\langle a,b \rangle` fell back to the raw
// "langle a, b rangle" because the glyph was absent from the symbol table.
func TestBareDelimiterMacros(t *testing.T) {
	cases := map[string][]rune{
		`\langle a, b \rangle`:                {'⟨', '⟩'},
		`\lfloor x \rfloor + \lceil y \rceil`: {'⌊', '⌋', '⌈', '⌉'},
	}
	for src, want := range cases {
		block, ok := Render2D(src, 80)
		if !ok {
			t.Errorf("Render2D(%q) fell back (ok=false), want 2D:\n%s", src, block)
			continue
		}
		for _, r := range want {
			if !strings.ContainsRune(block, r) {
				t.Errorf("Render2D(%q) missing %q:\n%s", src, r, block)
			}
		}
		if strings.Contains(block, "langle") || strings.Contains(block, "floor") || strings.Contains(block, "ceil") {
			t.Errorf("Render2D(%q) leaked a macro name:\n%s", src, block)
		}
	}
}

// An auto-sized \left…\right around a tall body draws the real delimiter glyph,
// never a │ bar substitute. Before the fix angle/floor/ceil fell through to
// DelimBar and a floor bracket rendered as a norm bar — a confident mis-render.
func TestLeftRightAngleNotBar(t *testing.T) {
	block, ok := Render2D(`\left\langle \frac{a}{b}, c \right\rangle`, 80)
	if !ok {
		t.Fatalf("Render2D fell back (ok=false):\n%s", block)
	}
	if !strings.ContainsRune(block, '⟨') || !strings.ContainsRune(block, '⟩') {
		t.Errorf("angle delimiters not drawn:\n%s", block)
	}
	if strings.ContainsRune(block, '│') {
		t.Errorf("angle pair drew a │ bar instead of ⟨⟩:\n%s", block)
	}
}

// A big operator over a multi-row operand stays a SINGLE glyph — only ∫ grows to
// the operand height (⌠⎮⌡). Before the fix ∑ over a fraction repeated the glyph
// down the column (∑∑∑), reading as several operators, not one enlarged sign.
func TestSumSingleGlyphOverTallBody(t *testing.T) {
	block, ok := Render2D(`\sum_{i=1}^{n}\frac{1}{i^2}`, 80)
	if !ok {
		t.Fatalf("Render2D fell back (ok=false):\n%s", block)
	}
	if n := strings.Count(block, "∑"); n != 1 {
		t.Errorf("∑ drawn %d times, want exactly 1 (single enlarged sign):\n%s", n, block)
	}
	// The integral, by contrast, IS extensible and grows over a tall body.
	iblock, _ := Render2D(`\int_{0}^{\infty}\frac{1}{x^2}dx`, 80)
	if !strings.ContainsRune(iblock, '⌠') {
		t.Errorf("∫ over a fraction should grow to the tall ⌠⎮⌡ form:\n%s", iblock)
	}
}

// A relation that opens an alignment cell keeps its trailing space: "&= y" must
// read "= y", not "=y". Before the fix layoutSeq only spaced an operator that
// was strictly interior, so a leading relation lost the space after it.
func TestAlignedRelationSpacing(t *testing.T) {
	block, ok := Render2D(`\begin{aligned} x &= y + z \\ a &= b \end{aligned}`, 80)
	if !ok {
		t.Fatalf("Render2D fell back (ok=false):\n%s", block)
	}
	if !strings.Contains(block, "x = y") {
		t.Errorf("aligned relation missing space after '=':\n%s", block)
	}
	// The same guard fix keeps a space after the '=' in an ordinary display line.
	m, _ := Render2D(`\nabla \cdot \mathbf{E} = \frac{\rho}{\varepsilon_0}`, 80)
	for _, ln := range strings.Split(m, "\n") {
		if i := strings.IndexRune(ln, '='); i >= 0 && i+len("=") < len(ln) {
			if next := ln[i+len("="):]; next != "" && !strings.HasPrefix(next, " ") {
				t.Errorf("no space after '=' in %q:\n%s", ln, m)
			}
		}
	}
}

// A superscript on an accented base floats ABOVE the accent glyph rather than
// sharing its row: \hat{x}^2 renders the 2 over the ^ over the x (three rows),
// not "^2 / x" where the caret and exponent collapse into one token.
func TestAccentScriptLift(t *testing.T) {
	block, ok := Render2D(`\hat{x}^2`, 80)
	if !ok {
		t.Fatalf("Render2D fell back (ok=false):\n%s", block)
	}
	lines := strings.Split(block, "\n")
	rowOf := func(r rune) int {
		for i, ln := range lines {
			if strings.ContainsRune(ln, r) {
				return i
			}
		}
		return -1
	}
	two, hat, x := rowOf('2'), rowOf('^'), rowOf('x')
	if two < 0 || hat < 0 || x < 0 {
		t.Fatalf("expected 2/^/x all present:\n%s", block)
	}
	if !(two < hat && hat < x) {
		t.Errorf("want 2 above ^ above x (rows %d/%d/%d):\n%s", two, hat, x, block)
	}
	for _, ln := range lines {
		if strings.ContainsRune(ln, '^') && strings.ContainsRune(ln, '2') {
			t.Errorf("accent and exponent collide on one row %q:\n%s", ln, block)
		}
	}
	// A subscript needs no lift — the accent is on top, the sub sits below.
	sub, ok := Render2D(`\bar{x}_i`, 80)
	if !ok || !strings.ContainsRune(sub, '─') || !strings.ContainsRune(sub, 'i') {
		t.Errorf("\\bar{x}_i should render bar over x with i below:\n%s", sub)
	}
}
