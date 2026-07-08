package chat

import (
	"io"
	"strings"
	"testing"

	"github.com/fatih/color"
	"github.com/rivo/uniseg"
)

// visible strips ANSI SGR codes, leaving the text the user actually sees.
func visible(s string) string { return ansiRe.ReplaceAllString(s, "") }

// sgrParams collects every parameter of every SGR sequence in s, so styles
// can be asserted regardless of how the params are grouped into sequences.
func sgrParams(s string) map[string]bool {
	params := map[string]bool{}
	for _, m := range ansiRe.FindAllString(s, -1) {
		for _, p := range strings.Split(strings.Trim(m, "\x1b[m"), ";") {
			params[p] = true
		}
	}
	return params
}

// renderMD runs src through a markdownWriter and returns the visible output.
func renderMD(t *testing.T, src string) string {
	t.Helper()
	var out strings.Builder
	m := newMarkdownWriter(&out)
	if _, err := m.Write([]byte(src)); err != nil {
		t.Fatalf("Write(%q): %v", src, err)
	}
	m.Flush()
	return visible(out.String())
}

// trimmedLines splits rendered output into lines with trailing padding spaces
// removed (lipgloss pads multi-line blocks to a uniform width).
func trimmedLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func assertLines(t *testing.T, got string, want []string) {
	t.Helper()
	lines := trimmedLines(got)
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), got)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q\n%s", i, lines[i], want[i], got)
		}
	}
}

func TestHighlightInlineHidesMarkers(t *testing.T) {
	color.NoColor = false // force SGR codes even though `go test` isn't a TTY
	tests := []struct {
		in, want string
	}{
		{"**bold**", "bold"},
		{"__bold__", "bold"},
		{"*italic*", "italic"},
		{"_italic_", "italic"},
		{"`code`", "code"},
		{"a **b** and `c`", "a b and c"},
		{"see [docs](http://x) ok", "see docs (http://x) ok"},
		{"plain text", "plain text"},
	}
	for _, tt := range tests {
		got := visible(highlightInline(tt.in))
		if got != tt.want {
			t.Errorf("highlightInline(%q) visible = %q, want %q", tt.in, got, tt.want)
		}
	}
	// The styling must still be applied (markers hidden, not just deleted).
	if !strings.Contains(highlightInline("**x**"), "\x1b[") {
		t.Errorf("bold span lost its styling")
	}
}

func TestTableRender(t *testing.T) {
	color.NoColor = false
	src := "| Name | Note |\n|------|------|\n| `--key` | the **secret** |\n| x | y |\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := visible(out.String())

	// Box-drawing borders are present.
	if !strings.Contains(got, "┌") || !strings.Contains(got, "│") {
		t.Errorf("table missing box-drawing:\n%s", got)
	}
	// Inline markers are hidden inside cells.
	for _, bad := range []string{"`--key`", "**secret**", "|---"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered table still shows markup %q:\n%s", bad, got)
		}
	}
	// Cell contents survive (markers stripped).
	for _, want := range []string{"--key", "secret", "Name", "Note"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered table missing %q:\n%s", want, got)
		}
	}
}

// TestTableAlignsEmojiAndCJK renders a table mixing plain wide emoji (🌍),
// VS16 emoji sequences (🌪️ ⚖️ ✈️ 🏛️ — a base char plus U+FE0F, which
// terminals draw 2 columns wide), CJK, and ASCII, and asserts the borders
// stay aligned under grapheme-cluster measurement. It also asserts the
// per-row rules: a ├─┼─┤ line between every pair of adjacent rows.
func TestTableAlignsEmojiAndCJK(t *testing.T) {
	color.NoColor = false
	src := "| Icon | Name | Note |\n" +
		"|------|------|------|\n" +
		"| 🌍 | Earth | plain wide |\n" +
		"| 🌪️ | Tornado | VS16 |\n" +
		"| ⚖️ | Scales | VS16 |\n" +
		"| ✈️ | Plane | VS16 |\n" +
		"| 🏛️ | Museum | VS16 |\n" +
		"| 中文 | 漢字テスト | CJK and ascii42 |\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := strings.TrimRight(visible(out.String()), "\n")
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("table too short:\n%s", got)
	}

	// Every line — borders and cell rows alike — must span the same number of
	// terminal columns (uniseg measures grapheme clusters, as terminals do).
	want := uniseg.StringWidth(lines[0])
	for _, ln := range lines {
		if w := uniseg.StringWidth(ln); w != want {
			t.Errorf("line width %d, want %d: %q\n%s", w, want, ln, got)
		}
	}

	// A rule row separates every pair of adjacent cell rows.
	cellRows, ruleRows := 0, 0
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "│"):
			cellRows++
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "│") {
				t.Errorf("missing rule between rows at line %d:\n%s", i, got)
			}
		case strings.HasPrefix(ln, "├"):
			ruleRows++
		}
	}
	if cellRows != 7 { // header + 6 data rows, one line each
		t.Errorf("cell rows = %d, want 7:\n%s", cellRows, got)
	}
	if ruleRows != cellRows-1 {
		t.Errorf("rule rows = %d, want %d:\n%s", ruleRows, cellRows-1, got)
	}
}

func TestCodeBlockRender(t *testing.T) {
	color.NoColor = false
	src := "```python\ndef f():\n    return 1\n```\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := out.String()

	if strings.Contains(got, "```") {
		t.Errorf("code fence not hidden:\n%q", got)
	}
	v := strings.TrimRight(visible(got), "\n")
	for _, ln := range strings.Split(v, "\n") {
		if !strings.HasPrefix(ln, "  ") {
			t.Errorf("code line not indented by two spaces: %q", ln)
		}
	}
	if !strings.Contains(v, "def f():") || !strings.Contains(v, "return 1") {
		t.Errorf("code content missing:\n%q", v)
	}
	if !strings.Contains(got, "\x1b[38;5;") {
		t.Errorf("code not syntax-highlighted:\n%q", got)
	}
}

// TestListNestedUnorderedHangingIndent checks two-level nesting, the hanging
// indent of a continuation line, and inline styling inside items.
func TestListNestedUnorderedHangingIndent(t *testing.T) {
	color.NoColor = false
	src := "- parent one\n" +
		"  wraps onto a second line\n" +
		"  - child one\n" +
		"  - `code` child\n" +
		"- parent two\n"
	assertLines(t, renderMD(t, src), []string{
		"• parent one",
		"  wraps onto a second line",
		"  • child one",
		"  • code child",
		"• parent two",
	})
}

// TestListOrderedStartOffset checks that ordered items keep their own numbers
// as written instead of being renumbered from 1.
func TestListOrderedStartOffset(t *testing.T) {
	color.NoColor = false
	src := "3. third\n4. fourth\n5. fifth\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	assertLines(t, visible(out.String()), []string{
		"3. third",
		"4. fourth",
		"5. fifth",
	})
	// The enumerator must be dimmed (SGR faint), like unordered bullets.
	if !strings.Contains(out.String(), "\x1b[2m") {
		t.Errorf("ordered marker not dimmed:\n%q", out.String())
	}
}

func TestListTaskGlyphs(t *testing.T) {
	color.NoColor = false
	src := "- [ ] write tests\n- [x] ship it\n"
	assertLines(t, renderMD(t, src), []string{
		"☐ write tests",
		"☑ ship it",
	})
}

// TestListLooseKeepsBlank checks that a blank line between items stays inside
// the block (loose list: blank between rendered items) while the blank that
// ends the list is re-emitted after it.
func TestListLooseKeepsBlank(t *testing.T) {
	color.NoColor = false
	src := "- alpha\n\n- beta\n\nafter paragraph\n"
	assertLines(t, renderMD(t, src), []string{
		"• alpha",
		"",
		"• beta",
		"",
		"after paragraph",
	})
}

// TestListFlushedByParagraph checks that a plain line flushes the block with
// no blank line invented in between (tight list).
func TestListFlushedByParagraph(t *testing.T) {
	color.NoColor = false
	src := "- one\n- two\nplain paragraph\n"
	assertLines(t, renderMD(t, src), []string{
		"• one",
		"• two",
		"plain paragraph",
	})
}

// TestListFlushOnUnterminated checks that Flush renders a list whose last
// line never saw a newline.
func TestListFlushOnUnterminated(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "- one\n- two"), []string{
		"• one",
		"• two",
	})
}

// TestListCJKWidth checks that CJK item text keeps its display width after
// the bullet prefix (no wrapping or padding surprises).
func TestListCJKWidth(t *testing.T) {
	color.NoColor = false
	got := trimmedLines(renderMD(t, "- 中文条目\n- ascii item\n"))
	if got[0] != "• 中文条目" {
		t.Errorf("line 0 = %q, want %q", got[0], "• 中文条目")
	}
	if w := uniseg.StringWidth(got[0]); w != 10 { // "• " (2) + four CJK runes (8)
		t.Errorf("line 0 width = %d, want 10", w)
	}
}

func TestHighlightLineHidesMarkers(t *testing.T) {
	m := newMarkdownWriter(io.Discard)
	tests := []struct {
		in, want string
	}{
		{"# Title", "Title"},
		{"### Deep heading", "Deep heading"},
		{"> quoted", "▌ quoted"},
		{"- item", "• item"},
		{"* item", "• item"},
		{"1. first", "1. first"},
		{"  - nested", "  • nested"},
	}
	for _, tt := range tests {
		got := visible(m.highlightLine(tt.in))
		if got != tt.want {
			t.Errorf("highlightLine(%q) visible = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestHeadingLevels checks the per-level heading styles — H1 bold+underline,
// H2 bold, H3 and deeper bold+faint — with the # markers hidden throughout.
func TestHeadingLevels(t *testing.T) {
	color.NoColor = false
	m := newMarkdownWriter(io.Discard)
	tests := []struct {
		in, text string
		want     []string // required SGR params (1 bold, 2 faint, 4 underline)
		absent   []string // forbidden SGR params
	}{
		{"# Top", "Top", []string{"1", "4"}, []string{"2"}},
		{"## Second", "Second", []string{"1"}, []string{"2", "4"}},
		{"### Third", "Third", []string{"1", "2"}, []string{"4"}},
		{"##### Fifth", "Fifth", []string{"1", "2"}, []string{"4"}},
	}
	for _, tt := range tests {
		got := m.highlightLine(tt.in)
		if v := visible(got); v != tt.text {
			t.Errorf("highlightLine(%q) visible = %q, want %q", tt.in, v, tt.text)
		}
		params := sgrParams(got)
		for _, p := range tt.want {
			if !params[p] {
				t.Errorf("highlightLine(%q) missing SGR %s: %q", tt.in, p, got)
			}
		}
		for _, p := range tt.absent {
			if params[p] {
				t.Errorf("highlightLine(%q) has unwanted SGR %s: %q", tt.in, p, got)
			}
		}
	}
}

// TestQuoteAndRuleDim checks that the blockquote bar and horizontal rules keep
// their dim rendering (a single faint SGR wrapping the whole line).
func TestQuoteAndRuleDim(t *testing.T) {
	color.NoColor = false
	m := newMarkdownWriter(io.Discard)
	tests := []struct{ in, want string }{
		{"> quoted", "\x1b[2m▌ quoted\x1b[0m"},
		{">", "\x1b[2m▌\x1b[0m"},
		{"---", "\x1b[2m---\x1b[0m"},
		{"* * *", "\x1b[2m* * *\x1b[0m"},
	}
	for _, tt := range tests {
		if got := m.highlightLine(tt.in); got != tt.want {
			t.Errorf("highlightLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestInlineSGRBytes pins the basic inline spans to the exact 16-color SGR
// bytes the fatih/color styles emitted before the lipgloss migration, so the
// two stacks stay byte-equivalent for simple styling.
func TestInlineSGRBytes(t *testing.T) {
	color.NoColor = false
	tests := []struct{ in, want string }{
		{"**b**", "\x1b[1mb\x1b[0m"},
		{"*i*", "\x1b[3mi\x1b[0m"},
		{"`c`", "\x1b[36mc\x1b[0m"},
	}
	for _, tt := range tests {
		if got := highlightInline(tt.in); got != tt.want {
			t.Errorf("highlightInline(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Link text is cyan+underline with a dim URL; lipgloss styles underlined
	// runs rune by rune, so assert on the params and visible text instead of
	// exact bytes.
	got := highlightInline("[docs](http://x)")
	if v := visible(got); v != "docs (http://x)" {
		t.Errorf("link visible = %q, want %q", v, "docs (http://x)")
	}
	params := sgrParams(got)
	for _, p := range []string{"36", "4", "2"} { // cyan, underline, dim URL
		if !params[p] {
			t.Errorf("link missing SGR %s: %q", p, got)
		}
	}
}

// TestMarkdownNoColorDisablesStyling checks the color.NoColor coupling: with
// the flag set, the whole markdown path — inline spans, headings, quotes,
// rules, lists, tables — must emit no escape codes at all, while markers are
// still hidden or replaced (styling off, layout intact).
func TestMarkdownNoColorDisablesStyling(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })

	src := "# Title\n" +
		"\n" +
		"**bold**, *it*, `code` and [docs](http://x)\n" +
		"\n" +
		"> quoted\n" +
		"\n" +
		"---\n" +
		"\n" +
		"- item one\n" +
		"- item two\n" +
		"\n" +
		"| A | B |\n" +
		"|---|---|\n" +
		"| 1 | 2 |\n"
	var out strings.Builder
	m := newMarkdownWriter(&out)
	if _, err := m.Write([]byte(src)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m.Flush()
	got := out.String()

	if strings.Contains(got, "\x1b") {
		t.Errorf("NoColor output contains escape codes:\n%q", got)
	}
	for _, want := range []string{"Title", "bold, it, code", "docs (http://x)", "▌ quoted", "---", "• item one", "┌"} {
		if !strings.Contains(got, want) {
			t.Errorf("NoColor output missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"# Title", "**bold**", "`code`", "> quoted", "- item"} {
		if strings.Contains(got, bad) {
			t.Errorf("NoColor output still shows markup %q:\n%s", bad, got)
		}
	}
}

// A tab inside a table cell must not swallow the content after it (tabs are
// normalized to a space at the parse boundary so every ruler agrees).
func TestTableCellTabPreserved(t *testing.T) {
	out := renderMD(t, "| H | K |\n|---|---|\n| a\tb | x |\n\n")
	plain := stripANSI(out)
	if !strings.Contains(plain, "a b") {
		t.Fatalf("tabbed cell content lost:\n%s", plain)
	}
}

// A table indented under a list item still renders as a bordered table (the
// list flushes first), matching the fence-under-list behavior.
func TestIndentedTableUnderListRenders(t *testing.T) {
	out := renderMD(t, "- item\n  | x | y |\n  |---|---|\n  | 1 | 2 |\n\n")
	plain := stripANSI(out)
	if !strings.Contains(plain, "┌") || !strings.Contains(plain, "┼") || !strings.Contains(plain, "│ 1") {
		t.Fatalf("indented table not rendered as a table:\n%s", plain)
	}
	if !strings.Contains(plain, "• item") {
		t.Fatalf("list item lost:\n%s", plain)
	}
}

// Table cells drop emoji presentation selectors (U+FE0F): terminals disagree
// on a VS16 sequence's cursor advance (Terminal.app 1, iTerm2 2), so bordered
// layouts only align everywhere with the bare base rune.
func TestTableStripsVariationSelectors(t *testing.T) {
	out := renderMD(t, "| C | N |\n|---|---|\n| ⚖️ 法庭 | x |\n\n")
	if strings.ContainsRune(out, '\uFE0F') {
		t.Fatal("rendered table still contains U+FE0F")
	}
	plain := stripANSI(out)
	if !strings.Contains(plain, "⚖ 法庭") {
		t.Fatalf("base rune lost:\n%s", plain)
	}
}

// Flag emoji pass through table cells untouched (user decision): a
// regional-indicator pair has no lossless narrow form, so terminals whose
// cursor advance disagrees with uniseg may misalign such rows — accepted.
func TestTableKeepsFlags(t *testing.T) {
	out := renderMD(t, "| C | N |\n|---|---|\n| \U0001F1EA\U0001F1FA \u6b27\u6d32 | x |\n\n")
	if !strings.ContainsRune(out, '\U0001F1EA') || !strings.ContainsRune(out, '\U0001F1FA') {
		t.Fatalf("flag emoji lost from table cell:\n%s", stripANSI(out))
	}
}
