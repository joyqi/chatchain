package mathtext

import (
	"strings"
	"testing"
)

func TestFindInlineDollar(t *testing.T) {
	src := "the value $x^2$ is nice"
	d, ok := FindInline(src, 0)
	if !ok {
		t.Fatalf("FindInline: no match")
	}
	if d.Body != "x^2" {
		t.Errorf("Body = %q, want %q", d.Body, "x^2")
	}
	if src[d.Start:d.End] != "$x^2$" {
		t.Errorf("span = %q, want %q", src[d.Start:d.End], "$x^2$")
	}
}

func TestFindInlineParen(t *testing.T) {
	src := `energy \(E = mc^2\) here`
	d, ok := FindInline(src, 0)
	if !ok {
		t.Fatalf("FindInline: no match")
	}
	if d.Body != "E = mc^2" {
		t.Errorf("Body = %q, want %q", d.Body, "E = mc^2")
	}
	if src[d.Start:d.End] != `\(E = mc^2\)` {
		t.Errorf("span = %q", src[d.Start:d.End])
	}
}

func TestFindInlineEscapedDollarLiteral(t *testing.T) {
	// A literal \$ must never open math.
	src := `it costs \$5 and \$10 total`
	if _, ok := FindInline(src, 0); ok {
		t.Errorf("FindInline matched a literal \\$ sequence")
	}
}

func TestFindInlineCurrencyGuard(t *testing.T) {
	// "$5 is cheap" — a digit opener is allowed, but there is no closing $,
	// so it stays literal.
	if _, ok := FindInline("$5 is cheap", 0); ok {
		t.Errorf("FindInline matched a currency $")
	}
	// "$ x" — space right after $ is currency-like / not math.
	if _, ok := FindInline("pay $ 5 now", 0); ok {
		t.Errorf("FindInline matched a $ followed by space")
	}
	// Unbalanced trailing $ is not math.
	if _, ok := FindInline("only $100", 0); ok {
		t.Errorf("FindInline matched an unbalanced trailing $")
	}
	// "$5 or $10" has two dollars, but the second is currency (a digit
	// follows), so it does not close — the pair never forms.
	if _, ok := FindInline("it costs $5 or $10", 0); ok {
		t.Errorf("FindInline paired two currency dollars into a span")
	}
	// "$20,000 and $30,000" — same: the closing candidate is followed by a
	// digit, so no span forms.
	if _, ok := FindInline("$20,000 and $30,000 total", 0); ok {
		t.Errorf("FindInline matched a thousands-separated currency pair")
	}
}

// TestFindInlineDigitOpener pins the fix for the regression where a formula
// starting with a digit (e.g. a Ramanujan "$1/\pi$" series) was misread as
// currency and left as raw LaTeX. A digit opener with a non-digit close is math.
func TestFindInlineDigitOpener(t *testing.T) {
	cases := map[string]string{
		`$1/\pi$ series`: `1/\pi`,
		`$2x+3$ here`:    `2x+3`,
		`at $0.5$ scale`: `0.5`,
	}
	for in, want := range cases {
		d, ok := FindInline(in, 0)
		if !ok {
			t.Errorf("FindInline(%q) = no match, want body %q", in, want)
			continue
		}
		if d.Body != want {
			t.Errorf("FindInline(%q) body = %q, want %q", in, d.Body, want)
		}
	}
}

// TestFindInlineAdversarial is the disambiguation corpus at the package layer:
// the boundary between a real inline span ($...$) and ordinary dollar usage
// (currency, price ranges, thousands separators, shell/prose variables). It
// mirrors chat.TestInlineMathVsDollarAdversarial but without the markdown
// code-span/escape context, so it excludes the `code` and \$ rows. The rule
// (opener not before a space; close neither before a digit nor after a space)
// must give exactly one classification per line: the first math span's body, or
// no span at all.
func TestFindInlineAdversarial(t *testing.T) {
	math := map[string]string{ // input -> first span body
		`$x$`:               `x`,
		`$1/\pi$ series`:    `1/\pi`,
		`$E=mc^2$`:          `E=mc^2`,
		`$2x + 3$`:          `2x + 3`,
		`$0.5$`:             `0.5`,
		`$\alpha + \beta$`:  `\alpha + \beta`,
		`$a_1$`:             `a_1`,
		`value $x=5$ ok`:    `x=5`,
		`$\frac{1}{2}$`:     `\frac{1}{2}`,
		`the $n$-th term`:   `n`,
		`$P(A|B)$`:          `P(A|B)`,
		`$x$ and $y$`:       `x`, // first span
		`$a + b$ and $c-d$`: `a + b`,
	}
	for in, want := range math {
		d, ok := FindInline(in, 0)
		if !ok {
			t.Errorf("FindInline(%q) = no span, want body %q", in, want)
			continue
		}
		if d.Body != want {
			t.Errorf("FindInline(%q) body = %q, want %q", in, d.Body, want)
		}
	}

	literal := []string{
		"It costs $5",
		"$5.00 total",
		"from $5 to $10",
		"$20,000 and $30,000",
		"prices $1, $2, $3",
		"the total is $100.",
		"$5-$10 range",
		"I paid $99 today",
		"echo $PATH",
		"$HOME/bin exists",
		"set $a and $b now",
		"run $CMD then $ARG",
		"$5 for the $x plan",
		"a $b and $c z",
		"cost $5, gain $x, net $y",
		"just $ alone",
		"ends with $",
		"$$x$$ fence",
		"$ x $ padded",
		"100$ suffix",
	}
	for _, in := range literal {
		if d, ok := FindInline(in, 0); ok {
			t.Errorf("FindInline(%q) matched span %q, want no span (literal dollars)", in, d.Body)
		}
	}
}

func TestFindInlineDisplayFenceNotInline(t *testing.T) {
	// $$...$$ on a run is a display fence, not an inline span.
	if _, ok := FindInline("$$x+y$$", 0); ok {
		t.Errorf("FindInline matched a $$ display fence as inline")
	}
}

func TestFindInlineNoNewline(t *testing.T) {
	// Inline math never spans a newline.
	if _, ok := FindInline("$a\nb$", 0); ok {
		t.Errorf("FindInline matched across a newline")
	}
}

func TestIsDisplayFence(t *testing.T) {
	yes := []string{"$$", "  $$  ", `\[`, `\]`}
	for _, s := range yes {
		if !IsDisplayFence(s) {
			t.Errorf("IsDisplayFence(%q) = false, want true", s)
		}
	}
	no := []string{"$$x$$", "$", "text", `\(`}
	for _, s := range no {
		if IsDisplayFence(s) {
			t.Errorf("IsDisplayFence(%q) = true, want false", s)
		}
	}
}

func TestStripDelimiters(t *testing.T) {
	cases := map[string]string{
		"$x+1$":     "x+1",
		"$$a=b$$":   "a=b",
		`\(y\)`:     "y",
		`\[z\]`:     "z",
		"  $ w $  ": "w",
		"no delim":  "no delim",
		"$":         "$", // too short to be a pair
	}
	for in, want := range cases {
		if got := StripDelimiters(in); got != want {
			t.Errorf("StripDelimiters(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCleanSource pins the Phase 3 fallback contract: CleanSource no longer dumps
// raw TeX but runs the single-line approximation, so greek/operators map to their
// glyph, \frac becomes a/b, \sqrt becomes √(…), and layout/spacing macros drop.
func TestCleanSource(t *testing.T) {
	cases := map[string]string{
		`$\frac{a}{b}$`:        "a/b",
		"$$  a  +   b  $$":     "a + b",
		`\( x \quad y \)`:      "x y",
		"plain   text   here":  "plain text here",
		`$\alpha + \beta$`:     "α + β",
		`\[ \sqrt{b^2-4ac} \]`: "√(b²-4ac)",
	}
	for in, want := range cases {
		if got := CleanSource(in); got != want {
			t.Errorf("CleanSource(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCleanSourceOutOfScopeReadable covers the display-math constructs the 2D
// engine punts to the fallback (\overbrace/\underbrace/\phantom + unknown
// macros). The fallback must read as math-ish text: NO leftover backslash-macro
// noise, and greek/frac/sqrt/scripts approximated. These are the formulas that
// still surface CleanSource in the terminal, so their output is load-bearing.
func TestCleanSourceOutOfScopeReadable(t *testing.T) {
	cases := map[string]string{
		`$$\overbrace{x + y + z}^{n}$$`:                   "x + y + zⁿ",
		`$$\phantom{x} + \alpha$$`:                        "+ α",
		`$$\underbrace{a + b}_{\text{sum}}$$`:             "a + bₛᵤₘ",
		`\[ \frac{-b \pm \sqrt{b^2-4ac}}{2a} \]`:          "(-b ± √(b²-4ac))/2a",
		`$$\mathcal{P}(S) = \{ T \mid T \subseteq S \}$$`: "𝒫(S) = { T ∣ T ⊆ S }",
	}
	for in, want := range cases {
		got := CleanSource(in)
		if got != want {
			t.Errorf("CleanSource(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsRune(got, '\\') {
			t.Errorf("CleanSource(%q) = %q still contains a backslash (raw TeX leaked)", in, got)
		}
	}
}

func TestIsInlineOpen(t *testing.T) {
	if !IsInlineOpen("$x$", 0) {
		t.Errorf("IsInlineOpen at a math $ = false")
	}
	// A digit opener now passes the cheap gate (FindInline validates the close);
	// only a "$" followed by a space is rejected outright.
	if !IsInlineOpen("$5", 0) {
		t.Errorf("IsInlineOpen at a digit $ = false, want true (gate only, close validated later)")
	}
	if IsInlineOpen("$ 5", 0) {
		t.Errorf("IsInlineOpen at a $ followed by space = true")
	}
	if IsInlineOpen("$$x", 0) {
		t.Errorf("IsInlineOpen at a display fence = true")
	}
	if !IsInlineOpen(`\(x`, 0) {
		t.Errorf(`IsInlineOpen at \( = false`)
	}
	if IsInlineOpen(`\[x`, 0) {
		t.Errorf(`IsInlineOpen at \[ = true`)
	}
}

// FindInline never treats a single-line display formula "$$...$$" as an inline
// span (both dollars of each "$$" run are skipped).
func TestFindInlineIgnoresDisplayFence(t *testing.T) {
	for _, in := range []string{"$$x$$", "$$x^2$$", "$$a+b$$", "$$"} {
		if d, ok := FindInline(in, 0); ok {
			t.Fatalf("FindInline(%q) = %+v, want no inline span", in, d)
		}
	}
}
