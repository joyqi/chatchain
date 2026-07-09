package mathtext

import "testing"

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
	// "$5 is cheap" — the $ is currency (digit follows), not math.
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

func TestCleanSource(t *testing.T) {
	cases := map[string]string{
		`$\frac{a}{b}$`:       `\frac{a}{b}`,
		"$$  a  +   b  $$":    "a + b",
		`\( x \quad y \)`:     `x \quad y`,
		"plain   text   here": "plain text here",
	}
	for in, want := range cases {
		if got := CleanSource(in); got != want {
			t.Errorf("CleanSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsInlineOpen(t *testing.T) {
	if !IsInlineOpen("$x$", 0) {
		t.Errorf("IsInlineOpen at a math $ = false")
	}
	if IsInlineOpen("$5", 0) {
		t.Errorf("IsInlineOpen at a currency $ = true")
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
