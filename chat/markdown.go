package chat

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"chatchain/internal/promptui"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// markdownWriter wraps an io.Writer and applies ANSI highlighting to markdown
// syntax elements line by line, without modifying the original text content.
type markdownWriter struct {
	w         io.Writer
	buf       []byte
	inFence   bool
	tableRows [][]string // buffered parsed cells per row
	tableSeps []bool     // true if row is a separator (|---|---|)
	fenceLang string     // language from the opening ``` fence
	codeLines []string   // buffered code-block lines, highlighted at the close
	// tableView / codeView show a live "rendering…" preview of a block while it
	// buffers (terminals only); the flush clears it before emitting the result.
	tableView *promptui.StreamView
	codeView  *promptui.StreamView
}

func newMarkdownWriter(w io.Writer) *markdownWriter {
	return &markdownWriter{w: w}
}

func (m *markdownWriter) Write(p []byte) (int, error) {
	m.buf = append(m.buf, p...)

	for {
		idx := indexOf(m.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(m.buf[:idx])
		m.buf = m.buf[idx+1:]

		// Fenced code block: buffer until the closing fence, then highlight once.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if m.inFence {
				m.inFence = false
				if err := m.flushCode(); err != nil {
					return len(p), err
				}
			} else {
				if len(m.tableRows) > 0 {
					if err := m.flushTable(); err != nil {
						return len(p), err
					}
				}
				m.inFence = true
				m.fenceLang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
				m.codeLines = nil
				m.codeView = newBlockPreview(m.w, codeLabel(m.fenceLang))
			}
			continue
		}
		if m.inFence {
			m.codeLines = append(m.codeLines, line)
			if m.codeView != nil {
				io.WriteString(m.codeView, line+"\n")
			}
			continue
		}

		if isTableLine(line) {
			if len(m.tableRows) == 0 {
				m.tableView = newBlockPreview(m.w, "rendering table…")
			}
			cells := parseTableCells(line)
			m.tableRows = append(m.tableRows, cells)
			m.tableSeps = append(m.tableSeps, isTableSeparator(cells))
			if m.tableView != nil {
				io.WriteString(m.tableView, line+"\n")
			}
			continue
		}

		if len(m.tableRows) > 0 {
			if err := m.flushTable(); err != nil {
				return len(p), err
			}
		}

		highlighted := m.highlightLine(line)
		if _, err := io.WriteString(m.w, highlighted+"\n"); err != nil {
			return len(p), err
		}
	}

	return len(p), nil
}

// Flush writes any remaining buffered content.
func (m *markdownWriter) Flush() {
	if m.inFence {
		// Unterminated code block: emit what we buffered.
		if len(m.buf) > 0 {
			m.codeLines = append(m.codeLines, string(m.buf))
			m.buf = nil
		}
		m.inFence = false
		m.flushCode()
		return
	}
	if len(m.tableRows) > 0 {
		m.flushTable()
	}
	if len(m.buf) > 0 {
		line := string(m.buf)
		m.buf = nil
		highlighted := m.highlightLine(line)
		io.WriteString(m.w, highlighted)
	}
}

func indexOf(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// highlightLine applies ANSI styles to a single line based on markdown syntax.
func (m *markdownWriter) highlightLine(line string) string {
	trimmed := strings.TrimSpace(line)

	// Heading: ## Title → drop the # markers, bold the text (with inline styling).
	if len(trimmed) > 0 && trimmed[0] == '#' {
		i := 0
		for i < len(trimmed) && trimmed[i] == '#' {
			i++
		}
		if i < len(trimmed) && trimmed[i] == ' ' {
			return BoldStyle.Sprint(strings.TrimSpace(trimmed[i:]))
		}
	}

	// Horizontal rule: --- or *** or ___
	if isHorizontalRule(trimmed) {
		return DimStyle.Sprint(line)
	}

	// Blockquote: > ... → a dim quote bar, the > marker hidden.
	if strings.HasPrefix(trimmed, "> ") {
		return DimStyle.Sprint("▌ " + trimmed[2:])
	}
	if trimmed == ">" {
		return DimStyle.Sprint("▌")
	}

	// List item: normalize the bullet (- * + → •) and dim it, style the rest.
	if marker, rest, ok := splitListMarker(line); ok {
		return renderListMarker(marker) + highlightInline(rest)
	}

	// Regular line: apply inline highlighting
	return highlightInline(line)
}

// highlightInline applies inline markdown styling and hides the markup itself:
// **bold**/__bold__ → bold text, *italic*/_italic_ → italic text, `code` →
// styled text (no backticks), and [text](url) → styled text + a dim URL.
func highlightInline(line string) string {
	var out strings.Builder
	runes := []rune(line)
	i := 0

	for i < len(runes) {
		// Link: [text](url) → styled text, brackets hidden, URL dimmed.
		if runes[i] == '[' {
			if textEnd := findClose(runes, i+1, ']'); textEnd > i+1 &&
				textEnd+1 < len(runes) && runes[textEnd+1] == '(' {
				if urlEnd := findClose(runes, textEnd+2, ')'); urlEnd > textEnd+1 {
					text := string(runes[i+1 : textEnd])
					url := string(runes[textEnd+2 : urlEnd])
					out.WriteString(LinkStyle.Sprint(text))
					out.WriteString(DimStyle.Sprint(" (" + url + ")"))
					i = urlEnd + 1
					continue
				}
			}
		}

		// Inline code: `code` → styled, backticks hidden.
		if runes[i] == '`' {
			if end := findClose(runes, i+1, '`'); end > 0 {
				out.WriteString(CodeStyle.Sprint(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}

		// Bold: **text** / __text__ → bold, markers hidden.
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			if end := findDoubleClose(runes, i+2, '*'); end > 0 {
				out.WriteString(BoldStyle.Sprint(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}
		if i+1 < len(runes) && runes[i] == '_' && runes[i+1] == '_' {
			if end := findDoubleClose(runes, i+2, '_'); end > 0 {
				out.WriteString(BoldStyle.Sprint(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}

		// Italic: *text* / _text_ → italic, markers hidden.
		// Avoid matching list bullets and horizontal rules.
		if runes[i] == '*' && i+1 < len(runes) && runes[i+1] != '*' && runes[i+1] != ' ' {
			if end := findClose(runes, i+1, '*'); end > i+1 {
				out.WriteString(ItalicStyle.Sprint(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}
		if runes[i] == '_' && i+1 < len(runes) && runes[i+1] != '_' && runes[i+1] != ' ' {
			// Only match if preceded by space or start of line.
			if i == 0 || unicode.IsSpace(runes[i-1]) {
				if end := findClose(runes, i+1, '_'); end > i+1 {
					out.WriteString(ItalicStyle.Sprint(string(runes[i+1 : end])))
					i = end + 1
					continue
				}
			}
		}

		out.WriteRune(runes[i])
		i++
	}

	return out.String()
}

// stripInlineMarkdown removes inline markdown delimiters (**, *, __, _, `)
// and returns plain text without any ANSI styling.
func stripInlineMarkdown(line string) string {
	var out strings.Builder
	runes := []rune(line)
	i := 0

	for i < len(runes) {
		if runes[i] == '`' {
			end := findClose(runes, i+1, '`')
			if end > 0 {
				out.WriteString(string(runes[i+1 : end]))
				i = end + 1
				continue
			}
		}
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			end := findDoubleClose(runes, i+2, '*')
			if end > 0 {
				out.WriteString(string(runes[i+2 : end]))
				i = end + 2
				continue
			}
		}
		if i+1 < len(runes) && runes[i] == '_' && runes[i+1] == '_' {
			end := findDoubleClose(runes, i+2, '_')
			if end > 0 {
				out.WriteString(string(runes[i+2 : end]))
				i = end + 2
				continue
			}
		}
		if runes[i] == '*' && i+1 < len(runes) && runes[i+1] != '*' && runes[i+1] != ' ' {
			end := findClose(runes, i+1, '*')
			if end > 0 && end > i+1 {
				out.WriteString(string(runes[i+1 : end]))
				i = end + 1
				continue
			}
		}
		if runes[i] == '_' && i+1 < len(runes) && runes[i+1] != '_' && runes[i+1] != ' ' {
			if i == 0 || unicode.IsSpace(runes[i-1]) {
				end := findClose(runes, i+1, '_')
				if end > 0 && end > i+1 {
					out.WriteString(string(runes[i+1 : end]))
					i = end + 1
					continue
				}
			}
		}
		out.WriteRune(runes[i])
		i++
	}
	return out.String()
}

// findClose finds the closing delimiter starting from pos.
func findClose(runes []rune, start int, delim rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == delim {
			return i
		}
	}
	return -1
}

// findDoubleClose finds a double closing delimiter (e.g., **) starting from pos.
func findDoubleClose(runes []rune, start int, delim rune) int {
	for i := start; i < len(runes)-1; i++ {
		if runes[i] == delim && runes[i+1] == delim {
			return i
		}
	}
	return -1
}

var listMarkerRe = regexp.MustCompile(`^(\s*(?:[-*+]|\d+[.)]) )`)

// splitListMarker splits a line into its list marker prefix and the rest.
func splitListMarker(line string) (marker, rest string, ok bool) {
	loc := listMarkerRe.FindStringIndex(line)
	if loc == nil {
		return "", "", false
	}
	return line[:loc[1]], line[loc[1]:], true
}

// renderListMarker styles a list marker: unordered bullets (- * +) become a dim
// "•", ordered markers (1. 2)) are kept but dimmed. Leading indentation is
// preserved so nested lists stay aligned.
func renderListMarker(marker string) string {
	bullet := strings.TrimLeft(marker, " \t")
	indent := marker[:len(marker)-len(bullet)]
	switch {
	case strings.HasPrefix(bullet, "- "), strings.HasPrefix(bullet, "* "), strings.HasPrefix(bullet, "+ "):
		return indent + DimStyle.Sprint("• ")
	default:
		return indent + DimStyle.Sprint(bullet)
	}
}

func isHorizontalRule(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	ch := rune(s[0])
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	for _, r := range s {
		if r != ch && r != ' ' {
			return false
		}
	}
	return true
}

// isTableLine returns true if the line looks like a markdown table row.
func isTableLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) > 0 && trimmed[0] == '|'
}

// parseTableCells splits a table line by | and trims each cell.
func parseTableCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	// Strip leading and trailing |
	if len(trimmed) > 0 && trimmed[0] == '|' {
		trimmed = trimmed[1:]
	}
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '|' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	parts := strings.Split(trimmed, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

var (
	tableSepRe = regexp.MustCompile(`^:?-+:?$`)
	ansiRe     = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	brRe       = regexp.MustCompile(`(?i)<br\s*/?>`)
)

// splitBR splits a string on <br>, <br/>, <BR> tags.
func splitBR(s string) []string {
	return brRe.Split(s, -1)
}

// isTableSeparator returns true if all cells match the separator pattern (e.g. ---, :--:).
func isTableSeparator(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !tableSepRe.MatchString(c) {
			return false
		}
	}
	return true
}

// flushTable renders the buffered table rows with aligned columns.
// If the table would exceed terminal width, columns are shrunk proportionally
// and cell text wraps within the cell across multiple visual lines.
// newBlockPreview opens a live rolling preview of a block's raw lines as they
// stream in, but only when writing to a terminal — off a terminal (pipe, tests)
// there is no cursor control, so the raw lines would just duplicate the rendered
// result. Returns nil when there is no terminal.
func newBlockPreview(w io.Writer, label string) *promptui.StreamView {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return &promptui.StreamView{
		Spinner:     spinnerFrames,
		Label:       label,
		HeaderStyle: dim,
		Window:      3,
		Indent:      "  ",
		RuneWidth:   runeWidth,
		Style:       dim,
		Stdout:      w,
	}
}

func codeLabel(lang string) string {
	if lang == "" {
		return "rendering code…"
	}
	return "rendering code (" + lang + ")…"
}

// flushCode clears the live preview and emits the buffered code block, syntax
// highlighted for the terminal.
func (m *markdownWriter) flushCode() error {
	if m.codeView != nil {
		m.codeView.Done("")
		m.codeView = nil
	}
	code := strings.Join(m.codeLines, "\n")
	lang := m.fenceLang
	m.codeLines = nil
	m.fenceLang = ""
	_, err := io.WriteString(m.w, highlightCode(code, lang))
	return err
}

// codeIndent is the uniform left margin added to every rendered code-block line,
// matching the buffering preview's indent.
const codeIndent = "  "

// highlightCode syntax-highlights a code block to ANSI via chroma, falling back
// to a plain block if highlighting fails (e.g. unknown content). Every line is
// indented by codeIndent.
func highlightCode(code, lang string) string {
	var sb strings.Builder
	if err := quick.Highlight(&sb, code, lang, "terminal256", codeStyleName()); err != nil {
		return indentCode(CodeBlockStyle.Sprint(code))
	}
	return indentCode(sb.String())
}

// indentCode prefixes every line of s with codeIndent. ANSI styles in chroma
// output reset at each token, so the indent (plain) never inherits a color.
func indentCode(s string) string {
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = codeIndent + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

var (
	codeTheme     = "monokai" // chroma style matching the current terminal background
	bgUnsupported bool        // terminal didn't answer OSC 11; stop re-querying
)

// codeStyleName returns the chroma style for the current terminal background. A
// light background gets a light theme so dark-on-light text stays readable;
// otherwise a dark theme. chroma's terminal256 formatter emits only foreground
// colors, so no background block is painted either way.
//
// Already-printed code keeps its baked-in colors — only new blocks pick up a
// background change.
func codeStyleName() string { return codeTheme }

// detectCodeTheme re-detects the terminal background (OSC 11 via termenv) and
// updates codeTheme. Call it only at quiet moments (startup, the start of a
// turn) — never mid-stream — so the query can't race user keystrokes. A
// responsive terminal answers in milliseconds, so per-turn re-detection is
// cheap and tracks light/dark switches; a terminal that ignores OSC 11 hits
// termenv's 5s timeout once, after which we latch off so later turns don't pay
// it again.
func detectCodeTheme() {
	if bgUnsupported {
		return
	}
	start := time.Now()
	dark := termenv.HasDarkBackground()
	if time.Since(start) > time.Second {
		bgUnsupported = true
	}
	if dark {
		codeTheme = "monokai"
	} else {
		codeTheme = "github"
	}
}

func (m *markdownWriter) flushTable() error {
	if m.tableView != nil {
		m.tableView.Done("") // clear the live preview before emitting the table
		m.tableView = nil
	}

	rows := m.tableRows
	seps := m.tableSeps
	m.tableRows = nil
	m.tableSeps = nil

	if len(rows) == 0 {
		return nil
	}

	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	// The header is the data row just before the first |---| separator.
	headerRow := -1
	for i, isSep := range seps {
		if isSep && i > 0 {
			headerRow = i - 1
			break
		}
	}

	// Natural display width per column (markers stripped, <br> split), then
	// proportionally shrink so the table fits the terminal; go-pretty wraps each
	// cell to these maxima (ANSI- and CJK-aware) and handles alignment/borders.
	colWidths := make([]int, maxCols)
	for _, row := range rows {
		for j := 0; j < maxCols && j < len(row); j++ {
			if w := cellDisplayWidth(row[j]); w > colWidths[j] {
				colWidths[j] = w
			}
		}
	}
	for j := range colWidths {
		if colWidths[j] < 3 {
			colWidths[j] = 3
		}
	}
	tw, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || tw <= 0 {
		tw = 80
	}
	overhead := 1 + maxCols*3 // leading border + " cell " + border per column
	available := tw - overhead
	if available < maxCols*3 {
		available = maxCols * 3
	}
	total := 0
	for _, w := range colWidths {
		total += w
	}
	if total > available {
		// Water-filling: columns that already fit their fair share keep their
		// natural width; only the wide columns shrink to absorb the deficit, so a
		// narrow column is never wrapped just because another column is huge.
		settled := make([]bool, maxCols)
		remaining, remainingCols := available, maxCols
		for {
			denom := remainingCols
			if denom < 1 {
				denom = 1
			}
			fair := remaining / denom
			changed := false
			for j := 0; j < maxCols; j++ {
				if !settled[j] && colWidths[j] <= fair {
					settled[j] = true
					remaining -= colWidths[j]
					remainingCols--
					changed = true
				}
			}
			if !changed || remainingCols == 0 {
				break
			}
		}
		if remainingCols > 0 {
			fair := remaining / remainingCols
			if fair < 3 {
				fair = 3
			}
			for j := 0; j < maxCols; j++ {
				if !settled[j] {
					colWidths[j] = fair
				}
			}
		}
	}

	t := table.NewWriter()
	t.SetStyle(table.StyleLight)
	st := t.Style()
	st.Format.Header = text.FormatDefault // keep header text as-is (no upper-casing)
	st.Color.Border = text.Colors{text.Faint}
	st.Color.Separator = text.Colors{text.Faint}
	st.Options.SeparateRows = false

	cfgs := make([]table.ColumnConfig, maxCols)
	for j := 0; j < maxCols; j++ {
		cfgs[j] = table.ColumnConfig{Number: j + 1, WidthMax: colWidths[j]}
	}
	t.SetColumnConfigs(cfgs)

	for i, row := range rows {
		if seps[i] {
			continue // markdown |---| row; go-pretty draws its own rules
		}
		cells := make(table.Row, maxCols)
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			if i == headerRow {
				cells[j] = headerCell(cell)
			} else {
				cells[j] = styledCell(cell)
			}
		}
		if i == headerRow {
			t.AppendHeader(cells)
		} else {
			t.AppendRow(cells)
		}
	}

	_, err = fmt.Fprintln(m.w, t.Render())
	return err
}

// styledCell renders a data cell's inline markdown (markers hidden) and turns
// <br> tags into newlines so go-pretty lays them out as multi-line cells.
func styledCell(cell string) string {
	segs := splitBR(cell)
	for i, s := range segs {
		segs[i] = highlightInline(strings.TrimSpace(s))
	}
	return strings.Join(segs, "\n")
}

// headerCell strips a header cell's inline markers and bolds each line. Each
// segment is bolded on its own so the bold never spans a newline (which would
// otherwise bleed into go-pretty's borders).
func headerCell(cell string) string {
	segs := splitBR(cell)
	for i, s := range segs {
		segs[i] = BoldStyle.Sprint(stripInlineMarkdown(strings.TrimSpace(s)))
	}
	return strings.Join(segs, "\n")
}

// cellDisplayWidth returns the visual display width of a table cell after markdown
// inline formatting is applied (stripping ANSI escape codes from the rendered result).
func cellDisplayWidth(cell string) int {
	maxW := 0
	for _, seg := range splitBR(cell) {
		w := displayWidth(stripInlineMarkdown(strings.TrimSpace(seg)))
		if w > maxW {
			maxW = w
		}
	}
	return maxW
}

// displayWidth returns the display width of a string, accounting for CJK characters.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth returns the display width of a rune (2 for CJK/fullwidth, 1 otherwise).
func runeWidth(r rune) int {
	if unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		(r >= 0x2E80 && r <= 0x2FDF) || // CJK Radicals Supplement, Kangxi Radicals
		(r >= 0x3000 && r <= 0x303F) || // CJK Symbols and Punctuation (、。〈〉 etc.)
		(r >= 0x3200 && r <= 0x33FF) || // Enclosed CJK Letters, CJK Compatibility
		(r >= 0xFE10 && r <= 0xFE1F) || // Vertical Forms
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK Compatibility Forms, Small Form Variants
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth forms
		(r >= 0xFFE0 && r <= 0xFFE6) { // Fullwidth signs
		return 2
	}
	return 1
}
