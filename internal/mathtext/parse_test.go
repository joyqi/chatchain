package mathtext

import (
	"errors"
	"testing"
)

// atomText asserts that n is an Atom with the given text.
func atomText(t *testing.T, n Node, want string) {
	t.Helper()
	a, ok := n.(*Atom)
	if !ok {
		t.Fatalf("node = %T, want *Atom", n)
	}
	if a.Text != want {
		t.Fatalf("Atom.Text = %q, want %q", a.Text, want)
	}
}

// seqOf asserts that n is a Seq with exactly want items and returns it.
func seqOf(t *testing.T, n Node, want int) *Seq {
	t.Helper()
	s, ok := n.(*Seq)
	if !ok {
		t.Fatalf("node = %T, want *Seq", n)
	}
	if len(s.Items) != want {
		t.Fatalf("Seq has %d items, want %d: %+v", len(s.Items), want, s.Items)
	}
	return s
}

func mustParse(t *testing.T, in string) Node {
	t.Helper()
	n, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", in, err)
	}
	return n
}

// TestParseFrac: \frac{a+b}{c} -> Frac{Num: Seq[a,+,b], Den: c}.
func TestParseFrac(t *testing.T) {
	n := mustParse(t, `\frac{a+b}{c}`)
	f, ok := n.(*Frac)
	if !ok {
		t.Fatalf("node = %T, want *Frac", n)
	}
	num := seqOf(t, f.Num, 3)
	atomText(t, num.Items[0], "a")
	atomText(t, num.Items[1], "+")
	atomText(t, num.Items[2], "b")
	atomText(t, f.Den, "c")
}

// TestParseSup: x^2 -> Sup{Base: x, Exp: 2}.
func TestParseSup(t *testing.T) {
	n := mustParse(t, `x^2`)
	s, ok := n.(*Sup)
	if !ok {
		t.Fatalf("node = %T, want *Sup", n)
	}
	atomText(t, s.Base, "x")
	atomText(t, s.Exp, "2")
}

// TestParseSub: a_i -> Sub{Base: a, Sub: i}.
func TestParseSub(t *testing.T) {
	n := mustParse(t, `a_i`)
	s, ok := n.(*Sub)
	if !ok {
		t.Fatalf("node = %T, want *Sub", n)
	}
	atomText(t, s.Base, "a")
	atomText(t, s.Sub, "i")
}

// TestParseSupSub: x_i^2 -> SupSub{Base: x, Sub: i, Sup: 2} (order-independent).
func TestParseSupSub(t *testing.T) {
	n := mustParse(t, `x_i^2`)
	ss, ok := n.(*SupSub)
	if !ok {
		t.Fatalf("node = %T, want *SupSub", n)
	}
	atomText(t, ss.Base, "x")
	atomText(t, ss.Sub, "i")
	atomText(t, ss.Sup, "2")

	// The other order a^b_c binds the same way.
	n2 := mustParse(t, `a^b_c`)
	ss2, ok := n2.(*SupSub)
	if !ok {
		t.Fatalf("node = %T, want *SupSub", n2)
	}
	atomText(t, ss2.Base, "a")
	atomText(t, ss2.Sup, "b")
	atomText(t, ss2.Sub, "c")
}

// TestParseSqrt: \sqrt{x+1} -> Sqrt{Radicand: Seq[x,+,1], Index: nil}.
func TestParseSqrt(t *testing.T) {
	n := mustParse(t, `\sqrt{x+1}`)
	s, ok := n.(*Sqrt)
	if !ok {
		t.Fatalf("node = %T, want *Sqrt", n)
	}
	if s.Index != nil {
		t.Fatalf("Sqrt.Index = %+v, want nil", s.Index)
	}
	rad := seqOf(t, s.Radicand, 3)
	atomText(t, rad.Items[0], "x")
	atomText(t, rad.Items[1], "+")
	atomText(t, rad.Items[2], "1")
}

// TestParseRootIndex: \sqrt[3]{x} -> Sqrt{Radicand: x, Index: 3}.
func TestParseRootIndex(t *testing.T) {
	n := mustParse(t, `\sqrt[3]{x}`)
	s, ok := n.(*Sqrt)
	if !ok {
		t.Fatalf("node = %T, want *Sqrt", n)
	}
	if s.Index == nil {
		t.Fatalf("Sqrt.Index = nil, want atom 3")
	}
	atomText(t, s.Index, "3")
	atomText(t, s.Radicand, "x")
}

// TestParseSum: \sum_{i=1}^{n} i -> BigOp{Op: sum, Lower: Seq[i,=,1], Upper: n}
// followed by the operand i, all inside a Seq.
func TestParseSum(t *testing.T) {
	n := mustParse(t, `\sum_{i=1}^{n} i`)
	s := seqOf(t, n, 2)
	op, ok := s.Items[0].(*BigOp)
	if !ok {
		t.Fatalf("first item = %T, want *BigOp", s.Items[0])
	}
	if op.Op != "sum" {
		t.Fatalf("BigOp.Op = %q, want sum", op.Op)
	}
	lower := seqOf(t, op.Lower, 3)
	atomText(t, lower.Items[0], "i")
	atomText(t, lower.Items[1], "=")
	atomText(t, lower.Items[2], "1")
	atomText(t, op.Upper, "n")
	atomText(t, s.Items[1], "i")
}

// TestParseInt: \int_0^1 f -> BigOp{Op: int, Lower: 0, Upper: 1} then f.
func TestParseInt(t *testing.T) {
	n := mustParse(t, `\int_0^1 f`)
	s := seqOf(t, n, 2)
	op, ok := s.Items[0].(*BigOp)
	if !ok {
		t.Fatalf("first item = %T, want *BigOp", s.Items[0])
	}
	if op.Op != "int" {
		t.Fatalf("BigOp.Op = %q, want int", op.Op)
	}
	atomText(t, op.Lower, "0")
	atomText(t, op.Upper, "1")
	atomText(t, s.Items[1], "f")
}

// TestParsePmatrix: \begin{pmatrix} a & b \\ c & d \end{pmatrix}.
func TestParsePmatrix(t *testing.T) {
	n := mustParse(t, `\begin{pmatrix} a & b \\ c & d \end{pmatrix}`)
	m, ok := n.(*Matrix)
	if !ok {
		t.Fatalf("node = %T, want *Matrix", n)
	}
	if m.Env != "pmatrix" {
		t.Fatalf("Matrix.Env = %q, want pmatrix", m.Env)
	}
	if len(m.Rows) != 2 || len(m.Rows[0]) != 2 || len(m.Rows[1]) != 2 {
		t.Fatalf("Matrix shape = %v, want 2x2", shape(m))
	}
	atomText(t, m.Rows[0][0], "a")
	atomText(t, m.Rows[0][1], "b")
	atomText(t, m.Rows[1][0], "c")
	atomText(t, m.Rows[1][1], "d")
}

// TestParseCases: \begin{cases} x & a \\ y & b \end{cases} -> two-column Matrix.
func TestParseCases(t *testing.T) {
	n := mustParse(t, `\begin{cases} x & a \\ y & b \end{cases}`)
	m, ok := n.(*Matrix)
	if !ok {
		t.Fatalf("node = %T, want *Matrix", n)
	}
	if m.Env != "cases" {
		t.Fatalf("Matrix.Env = %q, want cases", m.Env)
	}
	if len(m.Rows) != 2 || len(m.Rows[0]) != 2 || len(m.Rows[1]) != 2 {
		t.Fatalf("cases shape = %v, want 2x2", shape(m))
	}
	atomText(t, m.Rows[0][0], "x")
	atomText(t, m.Rows[0][1], "a")
	atomText(t, m.Rows[1][0], "y")
	atomText(t, m.Rows[1][1], "b")
}

// TestParseDelim: \left( \frac{a}{b} \right) -> Delim{(, ), Inner: Frac}.
func TestParseDelim(t *testing.T) {
	n := mustParse(t, `\left( \frac{a}{b} \right)`)
	d, ok := n.(*Delim)
	if !ok {
		t.Fatalf("node = %T, want *Delim", n)
	}
	if d.Left != "(" || d.Right != ")" {
		t.Fatalf("Delim = (%q,%q), want ( )", d.Left, d.Right)
	}
	f, ok := d.Inner.(*Frac)
	if !ok {
		t.Fatalf("Delim.Inner = %T, want *Frac", d.Inner)
	}
	atomText(t, f.Num, "a")
	atomText(t, f.Den, "b")
}

// TestParseText: \text{if } x -> Seq[Text{"if "}, x].
func TestParseText(t *testing.T) {
	n := mustParse(t, `\text{if } x`)
	s := seqOf(t, n, 2)
	tx, ok := s.Items[0].(*Text)
	if !ok {
		t.Fatalf("first item = %T, want *Text", s.Items[0])
	}
	if tx.S != "if " {
		t.Fatalf("Text.S = %q, want %q", tx.S, "if ")
	}
	atomText(t, s.Items[1], "x")
}

// TestParseGreek: \alpha -> Atom{"α"}.
func TestParseGreek(t *testing.T) {
	n := mustParse(t, `\alpha`)
	atomText(t, n, "α")
}

// TestParseAccent: \hat{f} -> Accent{Hat, Base: f}, and the bar/overline aliases
// share the AccentBar kind.
func TestParseAccent(t *testing.T) {
	cases := map[string]AccentKind{
		`\hat{f}`:       AccentHat,
		`\bar{x}`:       AccentBar,
		`\overline{ab}`: AccentBar,
		`\vec{v}`:       AccentVec,
		`\tilde{a}`:     AccentTilde,
		`\dot{x}`:       AccentDot,
		`\ddot{x}`:      AccentDdot,
	}
	for in, want := range cases {
		n := mustParse(t, in)
		a, ok := n.(*Accent)
		if !ok {
			t.Fatalf("Parse(%q) = %T, want *Accent", in, n)
		}
		if a.Kind != want {
			t.Errorf("Parse(%q) kind = %d, want %d", in, a.Kind, want)
		}
	}
	// \hat{f} accents an atom f.
	a := mustParse(t, `\hat{f}`).(*Accent)
	atomText(t, a.Base, "f")
}

// TestParseMathFont: \mathbb{R} -> Atom{ℝ}; \mathbf{E} -> Atom{E} (plain);
// \mathcal{L} -> Atom{ℒ}. A math-font macro never falls back.
func TestParseMathFont(t *testing.T) {
	atomText(t, mustParse(t, `\mathbb{R}`), "ℝ")
	atomText(t, mustParse(t, `\mathbb{C}`), "ℂ")
	atomText(t, mustParse(t, `\mathbb{N}`), "ℕ")
	atomText(t, mustParse(t, `\mathbf{E}`), "E")   // plain style degrades to itself
	atomText(t, mustParse(t, `\mathrm{d}`), "d")   // plain
	atomText(t, mustParse(t, `\mathcal{L}`), "ℒ")  // Letterlike hole
	atomText(t, mustParse(t, `\mathfrak{g}`), "𝔤") // systematic fraktur
	// Multi-letter arg maps each letter into a Seq of mapped atoms.
	s := seqOf(t, mustParse(t, `\mathbb{RC}`), 2)
	atomText(t, s.Items[0], "ℝ")
	atomText(t, s.Items[1], "ℂ")
}

// TestParseMid: \mid resolves to the ∣ relation glyph atom.
func TestParseMid(t *testing.T) {
	atomText(t, mustParse(t, `\mid`), "∣")
}

// TestParseLiteralBrace: \{ and \} are literal brace atoms (outside \left).
func TestParseLiteralBrace(t *testing.T) {
	s := seqOf(t, mustParse(t, `\{ x \}`), 3)
	atomText(t, s.Items[0], "{")
	atomText(t, s.Items[1], "x")
	atomText(t, s.Items[2], "}")
}

// TestParseNorm: \| is the norm bar ‖.
func TestParseNorm(t *testing.T) {
	atomText(t, mustParse(t, `\|`), "‖")
}

// TestParseAligned: \begin{aligned} a &= b \\ c &= d \end{aligned} parses into a
// two-column, two-row Matrix with Env "aligned".
func TestParseAligned(t *testing.T) {
	n := mustParse(t, `\begin{aligned} a &= b \\ c &= d \end{aligned}`)
	m, ok := n.(*Matrix)
	if !ok {
		t.Fatalf("node = %T, want *Matrix", n)
	}
	if m.Env != "aligned" {
		t.Fatalf("Matrix.Env = %q, want aligned", m.Env)
	}
	if len(m.Rows) != 2 || len(m.Rows[0]) != 2 || len(m.Rows[1]) != 2 {
		t.Fatalf("aligned shape = %v, want 2x2", shape(m))
	}
	atomText(t, m.Rows[0][0], "a")
	atomText(t, m.Rows[1][0], "c")
}

// TestParseAlignedRowBreakSpacing: a "\\[4pt]" spacing option after a row break
// is dropped, not treated as a stray token that forces a fallback.
func TestParseAlignedRowBreakSpacing(t *testing.T) {
	n := mustParse(t, `\begin{aligned} a &= b \\[4pt] c &= d \end{aligned}`)
	m, ok := n.(*Matrix)
	if !ok {
		t.Fatalf("node = %T, want *Matrix", n)
	}
	if len(m.Rows) != 2 {
		t.Fatalf("aligned with \\[4pt] shape = %v, want 2 rows", shape(m))
	}
}

// TestParseErrors covers unknown macros and Tier-3 constructs, each of which
// must return an error wrapping ErrUnsupported so the caller falls back.
func TestParseErrors(t *testing.T) {
	bad := []string{
		`\foobar`,                         // unknown macro
		`\overbrace{x}`,                   // Tier-3 accent-brace
		`\begin{matrixx} a \end{matrixx}`, // unknown environment
		`\phantom{x}`,                     // Tier-3 spacing
		`\frac{a}`,                        // missing second frac arg -> stray brace
		`{a`,                              // unbalanced group
		`a}`,                              // stray close brace
		`\left( a`,                        // \left with no \right
		``,                                // empty body
		`x^a^b`,                           // double superscript
		`\sqrt`,                           // missing radicand
	}
	for _, in := range bad {
		n, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, n)
			continue
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("Parse(%q) error %v does not wrap ErrUnsupported", in, err)
		}
	}
}

// shape returns the per-row cell counts of a matrix for error messages.
func shape(m *Matrix) []int {
	out := make([]int, len(m.Rows))
	for i, r := range m.Rows {
		out[i] = len(r)
	}
	return out
}
