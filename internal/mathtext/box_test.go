package mathtext

import (
	"reflect"
	"strings"
	"testing"
)

// wantBox asserts a box's rows, baseline, and width in one place, printing a
// side-by-side diff of the rendered grid on failure.
func wantBox(t *testing.T, got Box, wantLines []string, wantBaseline, wantWidth int) {
	t.Helper()
	if !reflect.DeepEqual(got.Lines, wantLines) {
		t.Errorf("lines mismatch\n got:\n%s\nwant:\n%s",
			strings.Join(got.Lines, "\n"), strings.Join(wantLines, "\n"))
	}
	if got.Baseline != wantBaseline {
		t.Errorf("baseline = %d, want %d", got.Baseline, wantBaseline)
	}
	if got.Width != wantWidth {
		t.Errorf("width = %d, want %d", got.Width, wantWidth)
	}
	// Every row must have the box's declared display width (the core invariant).
	for i, l := range got.Lines {
		if w := boxWidth(l); w != got.Width {
			t.Errorf("row %d width = %d, want %d (%q)", i, w, got.Width, l)
		}
	}
}

func TestNewBoxSingleLine(t *testing.T) {
	wantBox(t, NewBox("x"), []string{"x"}, 0, 1)
}

func TestNewBoxLinesPadsToWidth(t *testing.T) {
	// Multi-line text is padded to the widest row; baseline honored.
	got := NewBoxLines("ab\nc", 1)
	wantBox(t, got, []string{"ab", "c "}, 1, 2)
}

func TestBoxWidthCJK(t *testing.T) {
	// A box containing CJK is two columns per glyph: "中文" is width 4, and a
	// same-box row padding must reach that width in DISPLAY columns.
	b := NewBox("中文")
	if b.Width != 4 {
		t.Fatalf("CJK box width = %d, want 4", b.Width)
	}
	// Stacking a width-1 "x" under "中文" must pad "x" to 4 display columns.
	st := VStack([]Box{b, NewBox("x")}, 0)
	if st.Width != 4 {
		t.Fatalf("stacked width = %d, want 4", st.Width)
	}
	for _, l := range st.Lines {
		if boxWidth(l) != 4 {
			t.Errorf("row %q display width = %d, want 4", l, boxWidth(l))
		}
	}
}

func TestHConcatBaselineAlignment(t *testing.T) {
	// "x" (1 row, baseline 0) beside a 3-row box whose baseline is the middle
	// row must align x on that middle row, padding a blank row above and below.
	tall := Box{Lines: []string{"⎛", "⎜", "⎝"}, Baseline: 1, Width: 1}
	got := HConcat(NewBox("x"), tall)
	wantBox(t, got, []string{
		" ⎛",
		"x⎜",
		" ⎝",
	}, 1, 2)
}

func TestHConcatEmptyOperandsIgnored(t *testing.T) {
	got := HConcat(EmptyBox(), NewBox("a"), EmptyBox(), NewBox("b"))
	wantBox(t, got, []string{"ab"}, 0, 2)
}

func TestVStackFractionShape(t *testing.T) {
	// The canonical fraction: "a" over a bar over "b", baseline on the bar
	// (row 1). The bar is a drawn ─, and every row is centered to width 1.
	frac := VStack([]Box{NewBox("a"), HRule(1), NewBox("b")}, 1)
	wantBox(t, frac, []string{
		"a",
		"─",
		"b",
	}, 1, 1)
}

func TestVStackCentersWiderDenominator(t *testing.T) {
	// Numerator "1", bar sized to the widest part, denominator "x+y": the bar
	// spans width 3 and the numerator is centered over it.
	frac := VStack([]Box{NewBox("1"), HRule(3), NewBox("x+y")}, 1)
	wantBox(t, frac, []string{
		" 1 ",
		"───",
		"x+y",
	}, 1, 3)
}

func TestOverlineWidthMatchesAndDrawsRule(t *testing.T) {
	// Overline adds a drawn ─ row of the SAME width directly above; baseline
	// shifts down by one so it still points at the content row.
	over := Overline(NewBox("x+1"))
	wantBox(t, over, []string{
		"───",
		"x+1",
	}, 1, 3)
	if over.Width != NewBox("x+1").Width {
		t.Errorf("overline width %d != content width %d", over.Width, NewBox("x+1").Width)
	}
}

func TestHRule(t *testing.T) {
	wantBox(t, HRule(4), []string{"────"}, 0, 4)
	if HRule(0).Height() != 0 {
		t.Errorf("HRule(0) should be empty, got height %d", HRule(0).Height())
	}
}

func TestRaiseLowerBaseline(t *testing.T) {
	b := Box{Lines: []string{"p", "q", "r"}, Baseline: 1, Width: 1}
	if Raise(b, 1).Baseline != 0 {
		t.Errorf("Raise baseline = %d, want 0", Raise(b, 1).Baseline)
	}
	if Lower(b, 1).Baseline != 2 {
		t.Errorf("Lower baseline = %d, want 2", Lower(b, 1).Baseline)
	}
	// Clamped: cannot raise past the top row or lower past the bottom row.
	if Raise(b, 5).Baseline != 0 {
		t.Errorf("Raise over-clamp baseline = %d, want 0", Raise(b, 5).Baseline)
	}
	if Lower(b, 5).Baseline != 2 {
		t.Errorf("Lower over-clamp baseline = %d, want 2", Lower(b, 5).Baseline)
	}
}

func TestLeftDelimTallParen(t *testing.T) {
	// A 3-tall left paren renders the upper hook, extension, lower hook.
	got := LeftDelim(DelimParen, 3, 1)
	wantBox(t, got, []string{"⎛", "⎜", "⎝"}, 1, 1)
}

func TestRightDelimTallBracket(t *testing.T) {
	got := RightDelim(DelimBracket, 3, 1)
	wantBox(t, got, []string{"⎤", "⎥", "⎦"}, 1, 1)
}

func TestBraceDelimHasMiddlePiece(t *testing.T) {
	// A 5-tall left brace: top hook, extension, middle piece on the center row
	// (row 2), extension, bottom hook.
	got := LeftDelim(DelimBrace, 5, 2)
	wantBox(t, got, []string{"⎧", "⎪", "⎨", "⎪", "⎩"}, 2, 1)
}

func TestDelimHeightOneIsSingleGlyph(t *testing.T) {
	wantBox(t, LeftDelim(DelimParen, 1, 0), []string{"("}, 0, 1)
	wantBox(t, RightDelim(DelimParen, 1, 0), []string{")"}, 0, 1)
	wantBox(t, LeftDelim(DelimBracket, 1, 0), []string{"["}, 0, 1)
	wantBox(t, LeftDelim(DelimBrace, 1, 0), []string{"{"}, 0, 1)
	wantBox(t, LeftDelim(DelimBar, 1, 0), []string{"│"}, 0, 1)
}

func TestBarDelimIsAllVerticals(t *testing.T) {
	got := LeftDelim(DelimBar, 3, 1)
	wantBox(t, got, []string{"│", "│", "│"}, 1, 1)
}

func TestFractionInsideTallParens(t *testing.T) {
	// Integration of the primitives: place a fraction between auto-sized parens
	// aligned on the fraction's baseline (the bar). Proves HConcat aligns a
	// multi-row delimiter with a multi-row body on a shared baseline.
	frac := VStack([]Box{NewBox("a"), HRule(1), NewBox("b")}, 1)
	left := LeftDelim(DelimParen, frac.Height(), frac.Baseline)
	right := RightDelim(DelimParen, frac.Height(), frac.Baseline)
	got := HConcat(left, frac, right)
	wantBox(t, got, []string{
		"⎛a⎞",
		"⎜─⎟",
		"⎝b⎠",
	}, 1, 3)
}
