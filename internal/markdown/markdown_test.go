package markdown

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/fatih/color"
	"github.com/rivo/uniseg"
)

// newTestWriter builds a Writer over a plain buffer at the pre-extraction
// test width (80 — the old non-TTY fallback), no live previews.
func newTestWriter(w io.Writer) *Writer { return NewWriterTo(w, 80) }

// stripANSI removes ALL ANSI escapes — SGR and OSC (hyperlinks) alike.
func stripANSI(s string) string { return xansi.Strip(s) }

// visible strips ANSI escapes, leaving the text the user actually sees.
func visible(s string) string { return xansi.Strip(s) }

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

// renderMD runs src through a Writer and returns the visible output.
func renderMD(t *testing.T, src string) string {
	t.Helper()
	var out strings.Builder
	m := newTestWriter(&out)
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
	m := newTestWriter(&out)
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

// TestTableFlushOnUnterminated: a stream ending mid-table (the final row has
// no trailing newline) must fold that row into the rendered table — not close
// the table without it and print the raw "| … |" below the box.
func TestTableFlushOnUnterminated(t *testing.T) {
	color.NoColor = false
	src := "| Name | Code |\n|------|------|\n| Euro | EUR |\n| Aussie | AUD |"
	var out strings.Builder
	m := newTestWriter(&out)
	m.Write([]byte(src))
	m.Flush()
	got := visible(out.String())

	if !strings.Contains(got, "Aussie") || !strings.Contains(got, "AUD") {
		t.Errorf("unterminated final row missing from the table:\n%s", got)
	}
	if strings.Contains(got, "| Aussie") {
		t.Errorf("final row leaked as raw markdown below the table:\n%s", got)
	}
	// The row landed INSIDE the box: no content after the closing border.
	if i := strings.LastIndex(got, "└"); i >= 0 && strings.Contains(got[i:], "AUD") {
		t.Errorf("final row rendered outside the closing border:\n%s", got)
	}
}

// TestFlushPartialLineDispatch pins the unified Flush contract: the final
// partial line (no trailing newline) runs through the SAME dispatch as a
// terminated line, so every construct renders instead of leaking raw markdown
// — the drift class behind the table-last-row bug.
func TestFlushPartialLineDispatch(t *testing.T) {
	color.NoColor = false
	render := func(src string) string {
		var out strings.Builder
		m := newTestWriter(&out)
		m.Write([]byte(src))
		m.Flush()
		return visible(out.String())
	}

	// A final partial heading renders styled, not as raw "#" text.
	if got := render("body\n\n# Done"); strings.Contains(got, "# Done") || !strings.Contains(got, "Done") {
		t.Errorf("partial heading leaked raw:\n%s", got)
	}
	// A final partial list marker opens (and closes) a list: bullet rendered.
	if got := render("intro\n\n- item"); !strings.Contains(got, "• item") {
		t.Errorf("partial list item not rendered as a bullet:\n%s", got)
	}
	// A final partial one-line display formula renders as math, not raw "$$".
	if got := render("see:\n\n$$x^2$$"); strings.Contains(got, "$$") {
		t.Errorf("partial display math leaked raw:\n%s", got)
	}
	// A final partial closing fence ends the code block — no literal backticks.
	if got := render("```go\nfmt.Println(1)\n```"); strings.Contains(got, "```") {
		t.Errorf("partial closing fence leaked into the code block:\n%s", got)
	}
	// A final partial OPENING fence starts an empty block that closes clean —
	// no raw backticks, no crash.
	if got := render("text\n\n```go"); strings.Contains(got, "```") {
		t.Errorf("partial opening fence leaked raw:\n%s", got)
	}
	// A final partial quote line still renders with the quote bar.
	if got := render("> quoted"); !strings.Contains(got, "│") || strings.Contains(got, ">") {
		t.Errorf("partial quote line not framed:\n%s", got)
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
	m := newTestWriter(&out)
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
	m := newTestWriter(&out)
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
	m := newTestWriter(&out)
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

// TestListFlushedByParagraph checks that a plain line flushes the block and
// the state machine inserts exactly one blank line at the block→paragraph
// boundary, even though the source had no blank there (the new invariant:
// block-level elements are always bounded by one blank).
func TestListFlushedByParagraph(t *testing.T) {
	color.NoColor = false
	src := "- one\n- two\nplain paragraph\n"
	assertLines(t, renderMD(t, src), []string{
		"• one",
		"• two",
		"",
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
	m := newTestWriter(io.Discard)
	tests := []struct {
		in, want string
	}{
		{"# Title", "Title"},
		{"### Deep heading", "Deep heading"},
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
// every other level plain bold (never faint: models emit ###/#### constantly
// and faint-dominant terminals washed whole documents out) — with the #
// markers hidden throughout.
func TestHeadingLevels(t *testing.T) {
	color.NoColor = false
	m := newTestWriter(io.Discard)
	tests := []struct {
		in, text string
		want     []string // required SGR params (1 bold, 2 faint, 4 underline)
		absent   []string // forbidden SGR params
	}{
		{"# Top", "Top", []string{"1", "4"}, []string{"2"}},
		{"## Second", "Second", []string{"1"}, []string{"2", "4"}},
		{"### Third", "Third", []string{"1"}, []string{"2", "4"}},
		{"##### Fifth", "Fifth", []string{"1"}, []string{"2", "4"}},
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

// TestRuleDim checks that horizontal rules keep their dim rendering (a single
// faint SGR wrapping the whole line). Blockquotes are no longer a highlightLine
// concern (they render as a buffered block — see TestBlockquote*).
func TestRuleDim(t *testing.T) {
	color.NoColor = false
	m := newTestWriter(io.Discard)
	tests := []struct{ in, want string }{
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
	m := newTestWriter(&out)
	if _, err := m.Write([]byte(src)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m.Flush()
	got := out.String()

	if strings.Contains(got, "\x1b") {
		t.Errorf("NoColor output contains escape codes:\n%q", got)
	}
	for _, want := range []string{"Title", "bold, it, code", "docs (http://x)", "│ quoted", "---", "• item one", "┌"} {
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

// Inline markers inside headings are hidden, not rendered literally.
func TestHeadingStripsInlineMarkers(t *testing.T) {
	out := renderMD(t, "## **Bold** and `code` title\n\n")
	plain := stripANSI(out)
	if strings.Contains(plain, "**") || strings.Contains(plain, "`") {
		t.Fatalf("inline markers leaked into heading:\n%s", plain)
	}
	if !strings.Contains(plain, "Bold and code title") {
		t.Fatalf("heading text mangled:\n%s", plain)
	}
}

// renderMDRaw runs src through a Writer and returns the raw output,
// escape codes intact (renderMD strips them).
func renderMDRaw(t *testing.T, src string) string {
	t.Helper()
	var out strings.Builder
	m := newTestWriter(&out)
	if _, err := m.Write([]byte(src)); err != nil {
		t.Fatalf("Write(%q): %v", src, err)
	}
	m.Flush()
	return out.String()
}

// TestBlockquoteContinuousBar checks that a multi-line quote renders as one
// block with a continuous left bar: the │ glyph appears on every quote row
// (border-rune count == number of quote content rows), and an empty ">" line
// becomes an interior blank row that still carries the bar.
func TestBlockquoteContinuousBar(t *testing.T) {
	color.NoColor = false
	// four content rows: two text lines, one empty ">", one more text line.
	out := renderMD(t, "> first line\n> second line\n>\n> last line\n")
	lines := trimmedLines(out)
	if len(lines) != 4 {
		t.Fatalf("got %d quote rows, want 4:\n%s", len(lines), out)
	}
	bars := strings.Count(out, "│")
	if bars != 4 {
		t.Errorf("border rune count = %d, want 4 (one per content row):\n%s", bars, out)
	}
	// The interior blank row (from ">") carries only the bar, no text.
	if strings.TrimSpace(strings.TrimPrefix(lines[2], "│")) != "" {
		t.Errorf("empty > line is not a blank interior row: %q", lines[2])
	}
	for i, want := range []string{"first line", "second line", "", "last line"} {
		got := strings.TrimSpace(strings.TrimPrefix(lines[i], "│"))
		if got != want {
			t.Errorf("row %d text = %q, want %q", i, got, want)
		}
	}
}

// TestBlockquoteTextNotFaint checks the quote text is normal foreground (no
// lone faint SGR "2" wrapping it) while inline markdown is preserved: **bold**
// renders bold (SGR 1) with its "**" markers hidden. The bar may carry color 6.
func TestBlockquoteTextNotFaint(t *testing.T) {
	color.NoColor = false
	raw := renderMDRaw(t, "> a **bold** word and `code`\n")

	params := sgrParams(raw)
	if params["2"] {
		t.Errorf("quote text is faint (SGR 2 present), want normal foreground:\n%q", raw)
	}
	if !params["1"] {
		t.Errorf("inline bold inside quote lost its SGR 1:\n%q", raw)
	}
	if !params["6"] && !params["36"] {
		t.Errorf("quote bar missing its accent color (6):\n%q", raw)
	}
	plain := stripANSI(raw)
	if strings.Contains(plain, "**") || strings.Contains(plain, "`") {
		t.Errorf("inline markers leaked into quote:\n%s", plain)
	}
	if !strings.Contains(plain, "a bold word and code") {
		t.Errorf("quote text mangled:\n%s", plain)
	}
}

// TestBlockquoteInterruptedByParagraph checks the mutual flush wiring: a quote
// flushes on the first non-quote line, and the state machine inserts exactly
// one blank at the block→paragraph boundary (the new invariant bounds every
// block with one blank, even when the source had none).
func TestBlockquoteInterruptedByParagraph(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "> quoted\nplain paragraph\n"), []string{
		"│ quoted",
		"",
		"plain paragraph",
	})
}

// TestBlockquoteNoColorKeepsBar checks that with color.NoColor a blockquote
// still shows the │ bar but emits zero escape bytes (lipgloss draws the border
// glyph; the color drops).
func TestBlockquoteNoColorKeepsBar(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	raw := renderMDRaw(t, "> first\n> second\n")
	if strings.Contains(raw, "\x1b") {
		t.Errorf("NoColor blockquote contains escape codes:\n%q", raw)
	}
	if !strings.Contains(raw, "│") {
		t.Errorf("NoColor blockquote lost its bar:\n%q", raw)
	}
	if strings.Count(raw, "│") != 2 {
		t.Errorf("NoColor blockquote bar count = %d, want 2:\n%q", strings.Count(raw, "│"), raw)
	}
}

// TestBlankRunCollapse checks that a run of 2+ blank lines between paragraphs
// collapses to a single blank, and leading blank lines are suppressed.
func TestBlankRunCollapse(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "a\n\n\n\nb\n"), []string{
		"a",
		"",
		"b",
	})
	// Leading blanks before any content are dropped entirely.
	assertLines(t, renderMD(t, "\n\n\nfirst\n"), []string{
		"first",
	})
	// A single blank between paragraphs is preserved (pure collapse, not
	// re-spacing).
	assertLines(t, renderMD(t, "a\n\nb\n"), []string{
		"a",
		"",
		"b",
	})
}

// TestBlankRunCollapseCodeFencePreserved checks that blank lines inside a code
// fence are content and pass through verbatim (collapse applies only to the
// normal line path).
func TestBlankRunCollapseCodeFencePreserved(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	// Two blank lines between x and y must both survive; the normal-path
	// collapse must not reach into a fence. (Rendered code indents every line,
	// blank ones included, so blank rows appear as the indent prefix.)
	out := renderMD(t, "```\nx\n\n\ny\n```\n")
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	blanks := 0
	for _, r := range rows {
		if strings.TrimSpace(r) == "" {
			blanks++
		}
	}
	if blanks != 2 {
		t.Errorf("code-fence interior blank rows = %d, want 2 (not collapsed):\n%q", blanks, out)
	}
}

// TestBlankBetweenBlockAndParagraph checks that a single blank line between a
// list block and a following paragraph is preserved (not doubled, not removed):
// the block flush counts as non-blank output so the separator survives.
func TestBlankBetweenBlockAndParagraph(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "- one\n- two\n\nafter\n"), []string{
		"• one",
		"• two",
		"",
		"after",
	})
}

// blanksBetween counts the blank lines strictly between the first line whose
// trimmed text contains startNeedle and the next line whose trimmed text
// contains endNeedle. It fails the test if either anchor is missing or the end
// anchor does not follow the start anchor. Used to assert the "exactly one
// blank around every block" invariant on a document without hard-coding line
// numbers.
func blanksBetween(t *testing.T, rendered, startNeedle, endNeedle string) int {
	t.Helper()
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(strings.TrimSpace(ln), startNeedle) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("start anchor %q not found in:\n%s", startNeedle, rendered)
	}
	blanks := 0
	for i := start + 1; i < len(lines); i++ {
		if strings.Contains(strings.TrimSpace(lines[i]), endNeedle) {
			return blanks
		}
		if strings.TrimSpace(lines[i]) == "" {
			blanks++
		}
	}
	t.Fatalf("end anchor %q not found after %q in:\n%s", endNeedle, startNeedle, rendered)
	return -1
}

// TestBlockAdjacencyNoSourceBlanks feeds a document whose block-level elements
// (paragraph, list, heading, table) sit directly against each other with NO
// blank lines in the source, and asserts the state machine inserts exactly one
// blank line at every block boundary: no pair glued together, none doubled.
func TestBlockAdjacencyNoSourceBlanks(t *testing.T) {
	color.NoColor = false
	src := "A para\n" +
		"- item\n" +
		"- item\n" +
		"## Heading\n" +
		"Next para\n" +
		"| a | b |\n" +
		"|---|---|\n" +
		"| 1 | 2 |\n" +
		"Tail\n"
	out := renderMD(t, src)

	pairs := []struct{ start, end string }{
		{"A para", "item"},       // para → list
		{"item", "Heading"},      // list → heading
		{"Heading", "Next para"}, // heading → para
		{"Next para", "a"},       // para → table (table header cell "a")
		{"2", "Tail"},            // table → tail paragraph
	}
	for _, p := range pairs {
		if n := blanksBetween(t, out, p.start, p.end); n != 1 {
			t.Errorf("blank lines between %q and %q = %d, want 1:\n%s", p.start, p.end, n, out)
		}
	}
}

// TestParagraphIntegrity checks that three consecutive plain lines with NO
// blank lines between them render as one paragraph — three adjacent lines, no
// blank inserted anywhere (the whole subtlety of the state machine).
func TestParagraphIntegrity(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "line one\nline two\nline three\n"), []string{
		"line one",
		"line two",
		"line three",
	})
}

// TestBlankCollapseStillWorks checks a run of blank lines between two
// paragraphs collapses to exactly one blank.
func TestBlankCollapseStillWorks(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "a\n\n\nb"), []string{
		"a",
		"",
		"b",
	})
}

// TestHeadingBounding checks a heading between two paragraphs gets exactly one
// blank line above and below it, even with no source blanks.
func TestHeadingBounding(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "text\n## H\ntext\n"), []string{
		"text",
		"",
		"H",
		"",
		"text",
	})
}

// TestHorizontalRuleBounding checks a horizontal rule between two paragraphs
// gets exactly one blank line above and below it, even with no source blanks.
func TestHorizontalRuleBounding(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "text\n---\ntext\n"), []string{
		"text",
		"",
		"---",
		"",
		"text",
	})
}

// TestNoDanglingTrailingBlank checks a document ending in a block (a table)
// emits no trailing blank lines after it — the closing blank is produced lazily
// by the next unit's separator, and there is no next unit at EOF.
func TestNoDanglingTrailingBlank(t *testing.T) {
	color.NoColor = false
	out := renderMD(t, "para\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	if strings.HasSuffix(out, "\n\n") {
		t.Errorf("document ending in a block has a dangling trailing blank:\n%q", out)
	}
	// And the para → table boundary still has its single blank.
	if n := blanksBetween(t, out, "para", "a"); n != 1 {
		t.Errorf("blank lines between para and table = %d, want 1:\n%s", n, out)
	}
}

// TestBlockAdjacencyNoColor checks the adjacency document renders with zero
// escape bytes under color.NoColor while keeping the same block spacing (one
// blank at every boundary).
func TestBlockAdjacencyNoColor(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	src := "A para\n" +
		"- item\n" +
		"- item\n" +
		"## Heading\n" +
		"Next para\n" +
		"| a | b |\n" +
		"|---|---|\n" +
		"| 1 | 2 |\n" +
		"Tail\n"
	var out strings.Builder
	m := newTestWriter(&out)
	if _, err := m.Write([]byte(src)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m.Flush()
	raw := out.String()
	if strings.Contains(raw, "\x1b") {
		t.Errorf("NoColor adjacency output contains escape codes:\n%q", raw)
	}
	rendered := visible(raw)
	pairs := []struct{ start, end string }{
		{"A para", "item"},
		{"item", "Heading"},
		{"Heading", "Next para"},
		{"Next para", "a"},
		{"2", "Tail"},
	}
	for _, p := range pairs {
		if n := blanksBetween(t, rendered, p.start, p.end); n != 1 {
			t.Errorf("NoColor blank lines between %q and %q = %d, want 1:\n%s", p.start, p.end, n, rendered)
		}
	}
}

// A single-line quote with no trailing newline (a whole reply of "> x", or an
// interrupted stream) must still render the │ bar, not the raw ">".
func TestBlockquoteSingleLineNoTrailingNewline(t *testing.T) {
	plain := stripANSI(renderMD(t, "> just one line")) // note: no trailing newline
	if strings.Contains(plain, ">") {
		t.Fatalf("raw quote marker leaked:\n%q", plain)
	}
	if !strings.Contains(plain, "│") || !strings.Contains(plain, "just one line") {
		t.Fatalf("quote bar/text missing:\n%q", plain)
	}
}

// Blockquote inner content is parsed recursively: lists, headings, tables, and
// nested quotes inside a quote render as their block forms (not raw markdown),
// each row still fronted by the continuous │ bar.
func TestBlockquoteRecursiveBlocks(t *testing.T) {
	// List inside a quote → bullets, not raw "- ".
	list := stripANSI(renderMD(t, "> - one\n> - two\n\n"))
	if strings.Contains(list, "- one") || !strings.Contains(list, "•") {
		t.Fatalf("list not parsed inside quote:\n%s", list)
	}
	// Heading inside a quote → markers hidden.
	head := stripANSI(renderMD(t, "> ## Title\n> body\n\n"))
	if strings.Contains(head, "##") {
		t.Fatalf("heading marker leaked inside quote:\n%s", head)
	}
	// Table inside a quote → box drawing, not raw pipes.
	tbl := stripANSI(renderMD(t, "> | a | b |\n> |---|---|\n> | 1 | 2 |\n\n"))
	if !strings.Contains(tbl, "┌") || !strings.Contains(tbl, "┼") {
		t.Fatalf("table not parsed inside quote:\n%s", tbl)
	}
	// Nested quote → two bar columns on the inner line.
	nest := stripANSI(renderMD(t, "> outer\n> > inner\n\n"))
	if strings.Contains(nest, ">") {
		t.Fatalf("raw > leaked in nested quote:\n%s", nest)
	}
	if !strings.Contains(nest, "│ │") {
		t.Fatalf("nested quote lacks a second bar:\n%s", nest)
	}
	// Every rendered row is still fronted by the bar.
	for _, ln := range strings.Split(strings.TrimRight(tbl, "\n"), "\n") {
		if !strings.HasPrefix(ln, "│") {
			t.Fatalf("quote row without leading bar: %q", ln)
		}
	}
}

// A long blockquote paragraph that soft-wraps must carry the │ bar on EVERY
// wrapped row, while a table in the same quote stays intact (lipgloss wraps the
// overlong paragraph but leaves the already-fitted table lines alone).
func TestBlockquoteLongLineWraps(t *testing.T) {
	long := strings.Repeat("word ", 60) // ~300 cols, exceeds the 80-col default
	out := renderMD(t, "> "+long+"\n>\n> | a | b |\n> |---|---|\n> | 1 | 2 |\n\n")
	rows := strings.Split(strings.TrimRight(stripANSI(out), "\n"), "\n")
	if len(rows) < 4 {
		t.Fatalf("expected the long line to wrap into multiple rows:\n%s", stripANSI(out))
	}
	for i, ln := range rows {
		if !strings.HasPrefix(ln, "│") {
			t.Fatalf("row %d lost the bar: %q\nfull:\n%s", i, ln, stripANSI(out))
		}
	}
	// Table survived intact inside the quote.
	joined := stripANSI(out)
	if !strings.Contains(joined, "┌") || !strings.Contains(joined, "┼") || !strings.Contains(joined, "┘") {
		t.Fatalf("table mangled inside wrapped quote:\n%s", joined)
	}
}

// TestInlineMathApproximation checks the inline-math wiring: $...$ and \(...\)
// spans are replaced by their single-line Unicode approximation with the
// delimiters hidden, while the guarded non-math cases ($ escaped, unpaired,
// currency, inside a code span) stay literal.
func TestInlineMathApproximation(t *testing.T) {
	color.NoColor = false
	tests := []struct {
		name, in, want string
	}{
		{"greek", "$\\alpha$", "α"},
		{"superscript", "$x^2$", "x²"},
		{"paren-form", "\\(a+b\\)", "a+b"},
		{"in-sentence", "value is $\\beta$ here", "value is β here"},
		{"code-wins", "`$x$`", "$x$"},                        // code span beats math
		{"escaped-dollar", "\\$5", "$5"},                     // literal dollar
		{"unpaired", "cost is $5 today", "cost is $5 today"}, // no close → literal
		{"empty", "a $ $ b", "a $ $ b"},                      // empty body → not math
	}
	for _, tt := range tests {
		got := visible(highlightInline(tt.in))
		if got != tt.want {
			t.Errorf("%s: highlightInline(%q) visible = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
	// A math span must actually be styled (mdCode cyan), not merely stripped.
	if !strings.Contains(highlightInline("$x^2$"), "\x1b[") {
		t.Errorf("inline math span lost its styling")
	}
}

// TestInlineMathInTableCell checks that inline math inside a table cell renders
// on a single line (ApproxInline guarantees it) so the table stays aligned: no
// "$" leaks, the approximated glyph is present, and every rendered line spans
// the same terminal width.
func TestInlineMathInTableCell(t *testing.T) {
	color.NoColor = false
	out := renderMD(t, "| Sym | Val |\n|-----|-----|\n| $\\alpha$ | $x^2$ |\n| a | b |\n")
	plain := stripANSI(out)
	if strings.Contains(plain, "$") {
		t.Fatalf("dollar delimiter leaked into table cell:\n%s", plain)
	}
	if !strings.Contains(plain, "α") || !strings.Contains(plain, "x²") {
		t.Fatalf("inline math not approximated in table cell:\n%s", plain)
	}
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	want := uniseg.StringWidth(lines[0])
	for _, ln := range lines {
		if w := uniseg.StringWidth(ln); w != want {
			t.Errorf("table row width %d, want %d (math broke alignment): %q\n%s", w, want, ln, plain)
		}
	}
}

// TestInlineMathNoColor checks the color.NoColor coupling: a line containing
// inline math emits zero escape bytes while still approximating the formula and
// hiding the "$" delimiters.
func TestInlineMathNoColor(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	got := highlightInline("the value $x^2$ matters")
	if strings.Contains(got, "\x1b") {
		t.Errorf("NoColor inline math emitted escape codes:\n%q", got)
	}
	if got != "the value x² matters" {
		t.Errorf("NoColor inline math = %q, want %q", got, "the value x² matters")
	}
}

// The inline "$" path must never eat a "$$" display fence: on a line that is a
// display fence, highlightInline (the INLINE renderer, exercised directly here)
// leaves the "$$" untouched — the block writer, not the inline path, owns
// display math. This guards the P1 atomic-skip that keeps a one-line "$$x$$"
// from being mis-scanned into inline "$x$".
func TestInlineMathLeavesDisplayFence(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	for _, in := range []string{"$$x$$", "$$x^2$$", "$$a + b$$", "$$"} {
		got := highlightInline(in)
		if !strings.Contains(got, "$$") {
			t.Fatalf("inline path ate the display fence: %q → %q", in, got)
		}
	}
}

// renderMDChunked feeds src to a Writer in fixed-size byte chunks (to
// exercise the streaming path where a fence or formula line arrives split
// across Write calls) and returns the visible output.
func renderMDChunked(t *testing.T, src string, chunk int) string {
	t.Helper()
	var out strings.Builder
	m := newTestWriter(&out)
	b := []byte(src)
	for i := 0; i < len(b); i += chunk {
		end := i + chunk
		if end > len(b) {
			end = len(b)
		}
		if _, err := m.Write(b[i:end]); err != nil {
			t.Fatalf("Write chunk: %v", err)
		}
	}
	m.Flush()
	return visible(out.String())
}

// TestDisplayMathBlockMultiline checks a multi-line $$…$$ block renders as a 2D
// layout bounded by one blank line above and below (a block unit), with the
// fraction stacked over a drawn bar.
func TestDisplayMathBlockMultiline(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "before\n$$\n\\frac{a}{b}\n$$\nafter\n"), []string{
		"before",
		"",
		"  a",
		"  ─",
		"  b",
		"",
		"after",
	})
}

// TestDisplayMathOneLine checks the one-line "$$…$$" form renders the same 2D
// block as the multi-line fence form.
func TestDisplayMathOneLine(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "$$\\frac{a}{b}$$\n"), []string{
		"  a",
		"  ─",
		"  b",
	})
}

// TestDisplayMathBracketFence checks the "\[ … \]" fence form is recognized.
func TestDisplayMathBracketFence(t *testing.T) {
	color.NoColor = false
	assertLines(t, renderMD(t, "\\[\nx^2\n\\]\n"), []string{
		"   2",
		"  x",
	})
}

// TestDisplayMathInBlockquote checks a $$ block inside a blockquote fits inside
// the bar at the reduced inner width, and every rendered row keeps the │ bar.
func TestDisplayMathInBlockquote(t *testing.T) {
	color.NoColor = false
	out := renderMD(t, "> text\n> $$\n> \\frac{a}{b}\n> $$\n")
	lines := trimmedLines(out)
	for _, l := range lines {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "│") {
			t.Errorf("quoted math row lost its bar: %q\nfull:\n%s", l, out)
		}
	}
	// The fraction rows are inside the quote: the bar, then the indented layout.
	joined := strings.Join(lines, "\n")
	for _, frag := range []string{"a", "─", "b"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("quoted math lost %q:\n%s", frag, out)
		}
	}
}

// TestDisplayMathUnparseableFallback checks a Tier-3 construct in a $$ block
// falls back to the cleaned linear source rather than a 2D layout — and that the
// cleaned source reads as math-ish text (the \overbrace decoration is unwrapped
// to its inner content) with NO raw backslash-macro noise leaking to the
// terminal.
func TestDisplayMathUnparseableFallback(t *testing.T) {
	color.NoColor = false
	out := stripANSI(renderMD(t, "$$\n\\overbrace{x+y}\n$$\n"))
	if !strings.Contains(out, "x+y") {
		t.Errorf("unparseable math did not fall back to cleaned source:\n%s", out)
	}
	if strings.Contains(out, `\overbrace`) || strings.Contains(out, "\\") {
		t.Errorf("fallback leaked raw TeX to the terminal:\n%s", out)
	}
}

// TestDisplayMathUnparseableFallbackReadable checks the improved P3 fallback on a
// richer out-of-scope formula: an unsupported wrapper around a \frac and greek
// must surface the approximated "a/b" and glyphs, never the raw "\frac"/"\alpha".
func TestDisplayMathUnparseableFallbackReadable(t *testing.T) {
	color.NoColor = false
	out := stripANSI(renderMD(t, "$$\n\\overbrace{\\frac{a}{b} + \\alpha}\n$$\n"))
	if !strings.Contains(out, "a/b") {
		t.Errorf("fallback did not approximate \\frac to a/b:\n%s", out)
	}
	if !strings.Contains(out, "α") {
		t.Errorf("fallback did not map \\alpha to its glyph:\n%s", out)
	}
	if strings.Contains(out, `\frac`) || strings.Contains(out, `\alpha`) || strings.Contains(out, `\overbrace`) {
		t.Errorf("fallback leaked raw TeX macros:\n%s", out)
	}
}

// TestDisplayMathFallbackNotFaint checks the linear fallback for a formula the
// 2D engine can't lay out (\binom has no vertical form) renders in NORMAL color,
// not dimmed: the approximation is still the reader's formula, and dim is
// reserved for decoration. The C(n,k) fallback text must carry no faint SGR.
func TestDisplayMathFallbackNotFaint(t *testing.T) {
	color.NoColor = false
	raw := renderMDRaw(t, "$$\n(a+b)^n = \\sum_{k=0}^{n} \\binom{n}{k} a^{n-k} b^k\n$$\n")
	if sgrParams(raw)["2"] {
		t.Errorf("display-math fallback is faint (SGR 2 present), want normal foreground:\n%q", raw)
	}
	plain := stripANSI(raw)
	if !strings.Contains(plain, "C(n, k)") {
		t.Errorf("fallback did not approximate \\binom to C(n, k):\n%s", plain)
	}
	if strings.Contains(plain, "binom") || strings.Contains(plain, "\\") {
		t.Errorf("fallback leaked raw TeX:\n%s", plain)
	}
}

// TestDisplayMathStreamingMatchesOneShot checks byte-for-byte that feeding the
// source in 5-byte chunks produces the same output as one Write.
func TestDisplayMathStreamingMatchesOneShot(t *testing.T) {
	color.NoColor = false
	src := "intro\n$$\n\\frac{x+1}{y}\n$$\ndone\n"
	oneShot := renderMD(t, src)
	chunked := renderMDChunked(t, src, 5)
	if oneShot != chunked {
		t.Errorf("streaming != one-shot\none-shot:\n%q\nchunked:\n%q", oneShot, chunked)
	}
}

// TestDisplayMathNoColorZeroEsc checks that with color.NoColor a display-math
// block emits zero ANSI escape bytes (the 2D layout is glyph-based).
func TestDisplayMathNoColorZeroEsc(t *testing.T) {
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = false })
	raw := renderMDRaw(t, "$$\n\\frac{a}{b}\n$$\n")
	if strings.Contains(raw, "\x1b") {
		t.Errorf("NoColor display math emitted escape codes:\n%q", raw)
	}
	if !strings.Contains(raw, "─") {
		t.Errorf("NoColor display math lost its bar:\n%q", raw)
	}
}

// TestDisplayMathNoCombiningMarks checks that no rendered display block ever
// contains a Unicode combining mark (U+0300–U+036F) — the hard rule.
func TestDisplayMathNoCombiningMarks(t *testing.T) {
	color.NoColor = false
	srcs := []string{
		"$$\\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}$$\n",
		"$$\n\\begin{pmatrix} a & b \\\\ c & d \\end{pmatrix}\n$$\n",
		"$$\\sum_{i=1}^{n} \\frac{1}{i}$$\n",
	}
	for _, src := range srcs {
		out := renderMD(t, src)
		for _, r := range out {
			if r >= 0x0300 && r <= 0x036F {
				t.Errorf("display math emitted a combining mark U+%04X for %q:\n%s", r, src, out)
				break
			}
		}
	}
}

// TestInlineMathVsDollarAdversarial is the disambiguation corpus for the live
// inline path (highlightInline, with its code-span-first and \$-escape
// handling). It pins the boundary between real inline math ($...$) and ordinary
// dollar usage — currency, price ranges, thousands separators, shell/prose
// variables. The `want` is the exact plain (ANSI-stripped) render: math cases
// lose their delimiters, dollar cases survive byte-for-byte. See the pandoc-
// style rule in findInlineMath: opener not before a space, close neither before
// a digit ("$10") nor after a space ("and $b").
func TestInlineMathVsDollarAdversarial(t *testing.T) {
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = true })
	cases := []struct {
		in, want, note string
	}{
		// Real inline math — delimiters consumed, body approximated.
		{`$x$`, "x", "simple var"},
		{`$1/\pi$ series`, "1/π series", "digit opener + macro (the reported bug)"},
		{`$E=mc^2$`, "E=mc²", "superscript"},
		{`$2x + 3$`, "2x + 3", "digit opener, inner spaces"},
		{`$0.5$`, "0.5", "decimal opener"},
		{`$\alpha + \beta$`, "α + β", "greek"},
		{`$a_1$`, "a₁", "subscript"},
		{`value $x=5$ ok`, "value x=5 ok", "digit before close, then space"},
		{`$\frac{1}{2}$`, "1/2", "frac"},
		{`the $n$-th term`, "the n-th term", "single letter, hyphen after close"},
		{`$a + b$ and $c - d$`, "a + b and c - d", "two spans with inner spaces"},
		{`$P(A|B)$`, "P(A|B)", "pipe inside body"},
		{`$x$ and $y$`, "x and y", "two adjacent spans"},

		// Currency — must survive verbatim.
		{`It costs $5`, "It costs $5", "single price"},
		{`$5.00 total`, "$5.00 total", "decimal price"},
		{`from $5 to $10`, "from $5 to $10", "price range (classic pair trap)"},
		{`$20,000 and $30,000`, "$20,000 and $30,000", "thousands separators"},
		{`prices $1, $2, $3`, "prices $1, $2, $3", "three prices"},
		{`the total is $100.`, "the total is $100.", "price at end"},
		{`$5-$10 range`, "$5-$10 range", "dash range"},
		{`I paid $99 today`, "I paid $99 today", "mid-sentence price"},

		// Shell / prose variables and code — must survive.
		{`echo $PATH`, "echo $PATH", "single shell var (no close)"},
		{`$HOME/bin exists`, "$HOME/bin exists", "path var"},
		{`set $a and $b now`, "set $a and $b now", "two letter vars, space before close"},
		{`run $CMD then $ARG`, "run $CMD then $ARG", "two upper vars"},
		{`$5 for the $x plan`, "$5 for the $x plan", "price + var (digit-opener rule guard)"},
		{"use `$x$` inline", "use $x$ inline", "code span wins over math"},
		{`\$5 escaped`, "$5 escaped", "backslash-escaped dollar"},
		{`a $b and $c z`, "a $b and $c z", "letter var pair"},
		{`cost $5, gain $x, net $y`, "cost $5, gain $x, net $y", "price + two vars"},

		// Boundary / degenerate.
		{`just $ alone`, "just $ alone", "lone dollar with space"},
		{`ends with $`, "ends with $", "trailing dollar"},
		{`$$x$$ fence`, "$$x$$ fence", "display fence, not inline"},
		{`$ x $ padded`, "$ x $ padded", "space right after opener"},
		{`100$ suffix`, "100$ suffix", "dollar as suffix"},
	}
	for _, c := range cases {
		got := visible(highlightInline(c.in))
		if got != c.want {
			t.Errorf("[%s] highlightInline(%q) = %q, want %q", c.note, c.in, got, c.want)
		}
	}
}

// Inline containers nest: bold/italic/link text recurse with the container's
// attribute COMPOSED onto the segment style, so an inner span's reset can
// never cut the outer style mid-line, and no delimiter leaks.
func TestInlineNesting(t *testing.T) {
	color.NoColor = false
	syncMDRenderer()
	bold := mdPlain.Bold(true)
	boldCode := bold.Foreground(lipgloss.Color("6"))
	boldItalic := bold.Italic(true)

	got := highlightInline("**a `c` b**")
	want := bold.Render("a ") + boldCode.Render("c") + bold.Render(" b")
	if got != want {
		t.Errorf("bold∋code:\n got %q\nwant %q", got, want)
	}

	// The tail after the inner span must STILL be bold (the reset-cut class).
	if !strings.Contains(got, bold.Render(" b")) {
		t.Errorf("outer style lost after the inner span: %q", got)
	}

	if got, want := highlightInline("***x***"), boldItalic.Render("x"); got != want {
		t.Errorf("bold italic = %q, want %q", got, want)
	}

	// Code spans stay leaves: their content is literal, never re-parsed.
	if got := visible(highlightInline("`*args*`")); got != "*args*" {
		t.Errorf("code content re-parsed: %q", got)
	}

	// Link text nests; the URL stays a dim leaf.
	got = highlightInline("[see `x`](http://u)")
	linkCode := mdPlain.Foreground(lipgloss.Color("6")).Underline(true).Foreground(lipgloss.Color("6"))
	if !strings.Contains(got, linkCode.Render("x")) {
		t.Errorf("link∋code = %q, want code styled with the link underline", got)
	}
	if !strings.Contains(visible(got), "(http://u)") {
		t.Errorf("url lost: %q", visible(got))
	}
}

// Headings style their inline constructs on top of the heading look instead
// of stripping them (the strip was a workaround for nested resets).
func TestHeadingInlineStyled(t *testing.T) {
	color.NoColor = false
	var out strings.Builder
	m := newTestWriter(&out)
	m.Write([]byte("## Use `brew` now\n"))
	m.Flush()

	if got := visible(out.String()); !strings.Contains(got, "Use brew now") {
		t.Fatalf("heading text mangled: %q", got)
	}
	h2code := mdH2.Foreground(lipgloss.Color("6"))
	if !strings.Contains(out.String(), h2code.Render("brew")) {
		t.Errorf("heading code segment not composed (want bold+cyan): %q", out.String())
	}
}

// Hyperlinks are zero-width for the ANSI ruler (layout math must not count
// OSC 8 bytes), sanitize control bytes out of the URL, and pass text through
// bare under NoColor (no escapes into pipes).
func TestHyperlink(t *testing.T) {
	color.NoColor = false
	link := Hyperlink("file:///tmp/a.png", "a.png")
	if !strings.Contains(link, "\x1b]8;;file:///tmp/a.png") || !strings.Contains(link, "a.png") {
		t.Fatalf("link = %q", link)
	}
	if w := xansi.StringWidth(link); w != 5 {
		t.Fatalf("visible width = %d, want 5", w)
	}
	evil := Hyperlink("file:///a\x1bZ;rm -rf", "x")
	if !strings.Contains(evil, "]8;;file:///aZ;rm -rf") {
		t.Fatalf("control bytes not stripped from the URL: %q", evil)
	}
	color.NoColor = true
	defer func() { color.NoColor = false }()
	if got := Hyperlink("http://x", "plain"); got != "plain" {
		t.Fatalf("NoColor must pass through bare: %q", got)
	}
}

// Markdown links carry the OSC 8 wrapper around the styled text.
func TestLinkHyperlinked(t *testing.T) {
	color.NoColor = false
	got := highlightInline("[docs](http://x)")
	if !strings.Contains(got, "\x1b]8;;http://x") {
		t.Fatalf("link not hyperlinked: %q", got)
	}
	if !strings.Contains(xansi.Strip(got), "docs (http://x)") {
		t.Fatalf("visible text changed: %q", xansi.Strip(got))
	}
}

// TestTableNeverExactTerminalWidth: a table squeezed by the terminal must
// render at most width-1 columns wide. An exact-width row sits on the
// terminal's deferred-wrap boundary — downstream, the ui's overflow
// sanitizer trims one column off exact-multiple rows, which used to eat the
// table's right border.
func TestTableNeverExactTerminalWidth(t *testing.T) {
	color.NoColor = false
	long := strings.Repeat("wide content ", 20)
	for _, tw := range []int{40, 41, 80} {
		var out strings.Builder
		m := NewWriterTo(&out, tw)
		m.Write([]byte("| Col |\n|-----|\n| " + long + " |\n"))
		m.Flush()
		maxW, right := 0, false
		for _, ln := range strings.Split(strings.TrimRight(visible(out.String()), "\n"), "\n") {
			if w := displayWidth(ln); w > maxW {
				maxW = w
			}
			if strings.HasSuffix(strings.TrimRight(ln, " "), "│") ||
				strings.HasSuffix(strings.TrimRight(ln, " "), "┐") ||
				strings.HasSuffix(strings.TrimRight(ln, " "), "┘") ||
				strings.HasSuffix(strings.TrimRight(ln, " "), "┤") {
				right = true
			}
		}
		if maxW >= tw {
			t.Errorf("tw=%d: table row reaches %d columns — the exact-width boundary", tw, maxW)
		}
		if !right {
			t.Errorf("tw=%d: no right border found", tw)
		}
	}
}

// TestHighlightNeutralizesErrorTokens: lexers mark unparseable runs (CJK
// punctuation, prose in template literals) as Error, and most chroma styles
// paint Error with an alarm BACKGROUND (github: white on deep red) — seen
// live as garish blocks through normal code. Highlight renders Error tokens
// as plain text: no background sequence may survive, under either theme.
func TestHighlightNeutralizesErrorTokens(t *testing.T) {
	line := "重要: 这是一条发给**特定领域开发者**的推荐语（clarity、naturalness）"
	for _, style := range []string{"github", "monokai"} {
		var sb strings.Builder
		if err := Highlight(&sb, line, "JavaScript", style); err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		out := sb.String()
		if strings.Contains(out, "\x1b[48;") {
			t.Errorf("%s: background sequence leaked:\n%q", style, out)
		}
		if visible(out) != line {
			t.Errorf("%s: content mangled:\n%q", style, visible(out))
		}
	}
}
