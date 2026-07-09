package mathtext

import (
	"strings"
	"testing"
)

// TestRender2DSuccess: a parseable display formula returns a multi-line block
// with ok=true, matching the laid-out box rows joined by newlines.
func TestRender2DSuccess(t *testing.T) {
	block, ok := Render2D(`\frac{a}{b}`, 80)
	if !ok {
		t.Fatalf("Render2D ok=false for a valid fraction")
	}
	want := "a\n─\nb"
	if block != want {
		t.Errorf("Render2D = %q, want %q", block, want)
	}
}

// TestRender2DStripsDelimiters: the display fence around the body is stripped
// before parsing, so a "$$…$$" or "\[…\]" wrapper renders the same as the bare
// body.
func TestRender2DStripsDelimiters(t *testing.T) {
	bare, ok1 := Render2D(`x^2`, 80)
	fenced, ok2 := Render2D(`$$x^2$$`, 80)
	if !ok1 || !ok2 {
		t.Fatalf("Render2D ok = %v/%v, want true/true", ok1, ok2)
	}
	if bare != fenced {
		t.Errorf("fenced %q != bare %q", fenced, bare)
	}
}

// TestRender2DFallback: a Tier-3 construct that the parser rejects returns the
// cleaned linear source with ok=false so the caller degrades gracefully.
func TestRender2DFallback(t *testing.T) {
	for _, in := range []string{
		`\overbrace{x}`,                   // Tier-3 accent-brace
		`\begin{matrixx} a \end{matrixx}`, // unknown environment
		`\foobar`,                         // unknown macro
	} {
		block, ok := Render2D(in, 80)
		if ok {
			t.Errorf("Render2D(%q) ok=true, want fallback", in)
		}
		if block != CleanSource(in) {
			t.Errorf("Render2D(%q) fallback = %q, want %q", in, block, CleanSource(in))
		}
	}
}

// TestRender2DNoCombiningMarks: no rendered block ever carries a combining mark
// (U+0300–U+036F) — the load-bearing rule of the engine.
func TestRender2DNoCombiningMarks(t *testing.T) {
	for _, in := range []string{
		`\frac{-b \pm \sqrt{b^2 - 4ac}}{2a}`,
		`\sum_{i=1}^{n} \frac{1}{i^2}`,
		`\begin{pmatrix} \alpha & \beta \\ \gamma & \delta \end{pmatrix}`,
		`\sqrt[3]{\frac{x}{y}}`,
	} {
		block, _ := Render2D(in, 80)
		if hasCombiningMark(block) {
			t.Errorf("Render2D(%q) has a combining mark:\n%s", in, block)
		}
	}
}

// TestRender2DWideStillRenders: a formula wider than the budget is still
// returned with ok=true (best-effort overflow; shrink is a later phase).
func TestRender2DWideStillRenders(t *testing.T) {
	block, ok := Render2D(`\frac{aaaaaaaaaa+bbbbbbbbbb}{c}`, 5)
	if !ok {
		t.Fatalf("Render2D ok=false for a wide-but-valid formula")
	}
	if !strings.Contains(block, "─") {
		t.Errorf("wide fraction lost its bar:\n%s", block)
	}
}

// Big operators draw their own glyph, not a collapsed family glyph: a union
// must not render as a summation (regression — bigOpKind collapses families for
// tall shape, but the drawn glyph stays operator-specific).
func TestRender2DBigOpGlyphs(t *testing.T) {
	cases := map[string]rune{
		`\bigcup_{i} A_i`:   '⋃',
		`\bigcap_{i} A_i`:   '⋂',
		`\bigoplus_{i} V_i`: '⨁',
		`\coprod_{i} X_i`:   '∐',
		`\sum_{i} a_i`:      '∑',
		`\prod_{i} a_i`:     '∏',
	}
	for src, want := range cases {
		out, ok := Render2D(src, 80)
		if !ok {
			t.Errorf("Render2D(%q) fell back unexpectedly", src)
			continue
		}
		if !strings.ContainsRune(out, want) {
			t.Errorf("Render2D(%q) missing %q:\n%s", src, want, out)
		}
		if want != '∑' && strings.ContainsRune(out, '∑') {
			t.Errorf("Render2D(%q) drew ∑ instead of %q:\n%s", src, want, out)
		}
	}
}

// TestRender2DBattery is the Phase-3 real-world failure battery: every formula
// must render 2D (ok=true) and be free of combining marks. Items 3/5/6 already
// rendered in P2 (regression guard); 1/2/4/7/8 are the constructs this phase
// broadened (accents, \exp, \mid, math fonts, \{ \}, aligned environments).
func TestRender2DBattery(t *testing.T) {
	battery := []struct {
		name string
		src  string
		must []rune // key glyphs that prove the construct rendered
	}{
		{"fourier_hat", `\hat{f}(\xi) = \int_{-\infty}^{\infty} f(x) \, e^{-2\pi i x \xi} \, dx`, []rune{'^', '∫', 'ξ'}},
		{"gaussian_exp", `f(x \mid \mu, \sigma^2) = \frac{1}{\sigma\sqrt{2\pi}} \exp\left(-\frac{(x - \mu)^2}{2\sigma^2}\right)`, []rune{'∣', '╲', '─'}},
		{"navier_stokes", `\rho \left( \frac{\partial v}{\partial t} + v \cdot \nabla v \right) = -\nabla p + \mu \nabla^2 v + f`, []rune{'ρ', '∂', '∇'}},
		{"maxwell_aligned", `\begin{aligned} \nabla \cdot \mathbf{E} &= \frac{\rho}{\varepsilon_0} \\ \nabla \cdot \mathbf{B} &= 0 \end{aligned}`, []rune{'∇', 'ε', '─'}},
		{"derivative_lim", `\lim_{h \to 0} \frac{f(x+h) - f(x)}{h} = f'(x)`, []rune{'l', '→', '─'}},
		{"ftc_int", `\int f(x)dx = F(b) - F(a)`, []rune{'∫'}},
		{"set_intersection", `A \cap B = \{ x \mid x \in A \text{ and } x \in B \}`, []rune{'∩', '{', '∣', '}'}},
		{"powerset_mathcal", `\mathcal{P}(S) = \{ T \mid T \subseteq S \}`, []rune{'𝒫', '{', '∣', '⊆', '}'}},
	}
	for _, tc := range battery {
		block, ok := Render2D(tc.src, 80)
		if !ok {
			t.Errorf("%s: Render2D fell back (ok=false), want 2D render:\n%s", tc.name, block)
			continue
		}
		if hasCombiningMark(block) {
			t.Errorf("%s: rendered block has a combining mark:\n%s", tc.name, block)
		}
		for _, r := range tc.must {
			if !strings.ContainsRune(block, r) {
				t.Errorf("%s: rendered block missing %q:\n%s", tc.name, r, block)
			}
		}
	}
}

// TestRender2DNoCombiningMarkBattery greps the full output of every battery and
// probe formula for a combining mark (U+0300–U+036F): the set must be empty.
func TestRender2DNoCombiningMarkBattery(t *testing.T) {
	for _, in := range []string{
		`\hat{f}`, `\bar{x}`, `\vec{v}`, `\tilde{a}`, `\dot{x}`, `\ddot{x}`,
		`\overline{x + y}`, `\mathbb{R}`, `\mathcal{L}`, `\mathfrak{g}`,
		`\begin{aligned} a &= b \\ c &= d \end{aligned}`,
		`\{ x \mid x \in A \}`, `\| v \|`, `\exp(x)`,
		`\hat{f}(\xi) = \int_{-\infty}^{\infty} f(x) e^{-2\pi i x \xi} dx`,
	} {
		block, _ := Render2D(in, 80)
		for _, r := range block {
			if r >= 0x0300 && r <= 0x036F {
				t.Errorf("Render2D(%q) emitted combining mark U+%04X:\n%s", in, r, block)
				break
			}
		}
	}
}

// A pathologically long formula falls back to cleaned source rather than
// freezing the render on O(n^2) layout.
func TestRender2DOverlongFallsBack(t *testing.T) {
	huge := strings.Repeat("a+b+", 2000) + "c" // ~8000 runes, over the cap
	out, ok := Render2D(huge, 80)
	if ok {
		t.Fatal("overlong formula should fall back (ok=false)")
	}
	if !strings.Contains(out, "a+b+") {
		t.Fatalf("fallback should show cleaned source, got %d chars", len(out))
	}
}
