package chat

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

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

		if !m.inFence && isTableLine(line) {
			cells := parseTableCells(line)
			m.tableRows = append(m.tableRows, cells)
			m.tableSeps = append(m.tableSeps, isTableSeparator(cells))
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

	// Code fence toggle
	if strings.HasPrefix(trimmed, "```") {
		m.inFence = !m.inFence
		return DimStyle.Sprint(line)
	}

	// Inside code fence: green
	if m.inFence {
		return CodeBlockStyle.Sprint(line)
	}

	// Heading: # ... → bold
	if len(trimmed) > 0 && trimmed[0] == '#' {
		// Count consecutive # at start
		i := 0
		for i < len(trimmed) && trimmed[i] == '#' {
			i++
		}
		if i < len(trimmed) && trimmed[i] == ' ' {
			return BoldStyle.Sprint(line)
		}
	}

	// Horizontal rule: --- or *** or ___
	if isHorizontalRule(trimmed) {
		return DimStyle.Sprint(line)
	}

	// Blockquote: > ...
	if strings.HasPrefix(trimmed, "> ") || trimmed == ">" {
		return DimStyle.Sprint(line)
	}

	// List item: highlight the marker in dim
	if marker, rest, ok := splitListMarker(line); ok {
		return DimStyle.Sprint(marker) + highlightInline(rest)
	}

	// Regular line: apply inline highlighting
	return highlightInline(line)
}

// highlightInline applies inline markdown highlighting: **bold**, *italic*, `code`.
func highlightInline(line string) string {
	var out strings.Builder
	runes := []rune(line)
	i := 0

	for i < len(runes) {
		// Inline code: `...`
		if runes[i] == '`' {
			end := findClose(runes, i+1, '`')
			if end > 0 {
				out.WriteString(CodeStyle.Sprint(string(runes[i : end+1])))
				i = end + 1
				continue
			}
		}

		// Bold: **...** or __...__
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			end := findDoubleClose(runes, i+2, '*')
			if end > 0 {
				out.WriteString(BoldStyle.Sprint(string(runes[i : end+2])))
				i = end + 2
				continue
			}
		}
		if i+1 < len(runes) && runes[i] == '_' && runes[i+1] == '_' {
			end := findDoubleClose(runes, i+2, '_')
			if end > 0 {
				out.WriteString(BoldStyle.Sprint(string(runes[i : end+2])))
				i = end + 2
				continue
			}
		}

		// Italic: *...* or _..._
		// Avoid matching list bullets or horizontal rules
		if runes[i] == '*' && i+1 < len(runes) && runes[i+1] != '*' && runes[i+1] != ' ' {
			end := findClose(runes, i+1, '*')
			if end > 0 && end > i+1 {
				out.WriteString("\033[3m" + string(runes[i:end+1]) + "\033[23m")
				i = end + 1
				continue
			}
		}
		if runes[i] == '_' && i+1 < len(runes) && runes[i+1] != '_' && runes[i+1] != ' ' {
			// Only match if preceded by space or start of line
			if i == 0 || unicode.IsSpace(runes[i-1]) {
				end := findClose(runes, i+1, '_')
				if end > 0 && end > i+1 {
					out.WriteString("\033[3m" + string(runes[i:end+1]) + "\033[23m")
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
func (m *markdownWriter) flushTable() error {
	rows := m.tableRows
	seps := m.tableSeps
	m.tableRows = nil
	m.tableSeps = nil

	if len(rows) == 0 {
		return nil
	}

	// Determine max column count
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	// Calculate max display width per column
	colWidths := make([]int, maxCols)
	for _, row := range rows {
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			w := cellDisplayWidth(cell)
			if w > colWidths[j] {
				colWidths[j] = w
			}
		}
	}

	// Ensure minimum column width of 3
	for j := range colWidths {
		if colWidths[j] < 3 {
			colWidths[j] = 3
		}
	}

	// Get terminal width
	tw, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || tw <= 0 {
		tw = 80
	}

	// Shrink columns to fit terminal width if needed.
	// Overhead per row: 1 (leading │) + per column (1 space + cell + 1 space + 1 │)
	overhead := 1 + maxCols*3
	available := tw - overhead
	if available < maxCols*3 {
		available = maxCols * 3
	}
	totalContent := 0
	for _, w := range colWidths {
		totalContent += w
	}
	if totalContent > available {
		newWidths := make([]int, maxCols)
		for j, w := range colWidths {
			newWidths[j] = w * available / totalContent
			if newWidths[j] < 3 {
				newWidths[j] = 3
			}
		}
		colWidths = newWidths
	}

	// Identify header row: the row before the first separator
	headerRow := -1
	for i, isSep := range seps {
		if isSep && i > 0 {
			headerRow = i - 1
			break
		}
	}

	// Helper to render a horizontal divider line
	renderDivider := func(heavy bool) string {
		var line strings.Builder
		ch := "─"
		cross := "┼"
		if heavy {
			ch = "━"
			cross = "╋"
		}
		line.WriteString(DimStyle.Sprint("│"))
		for j := 0; j < maxCols; j++ {
			line.WriteString(DimStyle.Sprint(" " + strings.Repeat(ch, colWidths[j]) + " "))
			if j < maxCols-1 {
				line.WriteString(DimStyle.Sprint(cross))
			}
		}
		line.WriteString(DimStyle.Sprint("│"))
		return line.String()
	}

	prevWasData := false

	for i, row := range rows {
		if seps[i] {
			// Separator after header uses heavy line; skip original separator
			if _, err := fmt.Fprintln(m.w, renderDivider(i == headerRow+1)); err != nil {
				return err
			}
			prevWasData = false
			continue
		}

		isHeader := i == headerRow

		// Light divider between data rows
		if prevWasData && !isHeader {
			if _, err := fmt.Fprintln(m.w, renderDivider(false)); err != nil {
				return err
			}
		}

		// Parse each cell into styled spans, then wrap into visual lines.
		// Split on <br> first to handle model-generated HTML line breaks.
		wrappedCells := make([][]string, maxCols)
		maxLines := 1
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			// Split cell on <br> tags (case-insensitive, with optional space)
			segments := splitBR(cell)
			var allLines []string
			for _, seg := range segments {
				seg = strings.TrimSpace(seg)
				spans := parseInlineSpans(seg)
				if isHeader {
					for k := range spans {
						spans[k].start = "\033[1m"
						spans[k].end = "\033[22m"
					}
				}
				allLines = append(allLines, wrapSpans(spans, colWidths[j])...)
			}
			if len(allLines) == 0 {
				allLines = []string{""}
			}
			wrappedCells[j] = allLines
			if len(allLines) > maxLines {
				maxLines = len(allLines)
			}
		}

		// Render visual lines for this row
		for ln := 0; ln < maxLines; ln++ {
			var line strings.Builder
			line.WriteString(DimStyle.Sprint("│"))
			for j := 0; j < maxCols; j++ {
				rendered := ""
				visWidth := 0
				if ln < len(wrappedCells[j]) {
					rendered = wrappedCells[j][ln]
					visWidth = displayWidth(ansiRe.ReplaceAllString(rendered, ""))
				}
				pad := colWidths[j] - visWidth
				if pad < 0 {
					pad = 0
				}
				line.WriteString(" " + rendered + strings.Repeat(" ", pad) + " ")
				if j < maxCols-1 {
					line.WriteString(DimStyle.Sprint("│"))
				}
			}
			line.WriteString(DimStyle.Sprint("│"))
			if _, err := fmt.Fprintln(m.w, line.String()); err != nil {
				return err
			}
		}
		prevWasData = !isHeader
	}
	return nil
}

// styledSpan represents a piece of text with an optional ANSI style.
type styledSpan struct {
	text  string
	start string // ANSI escape to start style (empty for plain)
	end   string // ANSI escape to end style
}

// parseInlineSpans parses inline markdown into styled spans, stripping delimiters.
func parseInlineSpans(line string) []styledSpan {
	var spans []styledSpan
	runes := []rune(line)
	i := 0
	var plain []rune

	flushPlain := func() {
		if len(plain) > 0 {
			spans = append(spans, styledSpan{text: string(plain)})
			plain = nil
		}
	}

	for i < len(runes) {
		// Inline code: `...`
		if runes[i] == '`' {
			end := findClose(runes, i+1, '`')
			if end > 0 {
				flushPlain()
				spans = append(spans, styledSpan{
					text:  string(runes[i+1 : end]),
					start: CodeStyle.SprintFunc()("")[0:0], // we'll use Sprint directly
				})
				// Use a simpler approach: store style name and render later
				// Actually just store the ANSI codes
				spans[len(spans)-1] = styledSpan{
					text:  string(runes[i+1 : end]),
					start: "\033[36m", // cyan (CodeStyle)
					end:   "\033[0m",
				}
				i = end + 1
				continue
			}
		}

		// Bold: **...**
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			end := findDoubleClose(runes, i+2, '*')
			if end > 0 {
				flushPlain()
				spans = append(spans, styledSpan{
					text:  string(runes[i+2 : end]),
					start: "\033[1m",
					end:   "\033[22m",
				})
				i = end + 2
				continue
			}
		}
		// Bold: __...__
		if i+1 < len(runes) && runes[i] == '_' && runes[i+1] == '_' {
			end := findDoubleClose(runes, i+2, '_')
			if end > 0 {
				flushPlain()
				spans = append(spans, styledSpan{
					text:  string(runes[i+2 : end]),
					start: "\033[1m",
					end:   "\033[22m",
				})
				i = end + 2
				continue
			}
		}

		// Italic: *...*
		if runes[i] == '*' && i+1 < len(runes) && runes[i+1] != '*' && runes[i+1] != ' ' {
			end := findClose(runes, i+1, '*')
			if end > 0 && end > i+1 {
				flushPlain()
				spans = append(spans, styledSpan{
					text:  string(runes[i+1 : end]),
					start: "\033[3m",
					end:   "\033[23m",
				})
				i = end + 1
				continue
			}
		}
		// Italic: _..._
		if runes[i] == '_' && i+1 < len(runes) && runes[i+1] != '_' && runes[i+1] != ' ' {
			if i == 0 || unicode.IsSpace(runes[i-1]) {
				end := findClose(runes, i+1, '_')
				if end > 0 && end > i+1 {
					flushPlain()
					spans = append(spans, styledSpan{
						text:  string(runes[i+1 : end]),
						start: "\033[3m",
						end:   "\033[23m",
					})
					i = end + 1
					continue
				}
			}
		}

		plain = append(plain, runes[i])
		i++
	}
	flushPlain()
	return spans
}

// wrapSpans wraps styled spans into visual lines of at most maxWidth display columns.
// Each returned string is already ANSI-styled and ready to print.
func wrapSpans(spans []styledSpan, maxWidth int) []string {
	if maxWidth <= 0 {
		// No wrapping possible, render everything on one line
		var out strings.Builder
		for _, sp := range spans {
			out.WriteString(sp.start + sp.text + sp.end)
		}
		return []string{out.String()}
	}

	var lines []string
	var cur strings.Builder
	curWidth := 0

	for _, sp := range spans {
		runes := []rune(sp.text)
		ri := 0
		for ri < len(runes) {
			// Start a styled segment on the current line
			if sp.start != "" {
				cur.WriteString(sp.start)
			}
			wrote := false
			for ri < len(runes) {
				rw := runeWidth(runes[ri])
				if curWidth+rw > maxWidth {
					break
				}
				cur.WriteRune(runes[ri])
				curWidth += rw
				ri++
				wrote = true
			}
			if sp.start != "" {
				cur.WriteString(sp.end)
			}

			// If we couldn't fit any rune and line is empty, force one rune
			if !wrote && curWidth == 0 {
				if sp.start != "" {
					cur.WriteString(sp.start)
				}
				cur.WriteRune(runes[ri])
				curWidth += runeWidth(runes[ri])
				ri++
				if sp.start != "" {
					cur.WriteString(sp.end)
				}
			}

			// If there are more runes in this span or line is full, break line
			if ri < len(runes) {
				lines = append(lines, cur.String())
				cur.Reset()
				curWidth = 0
			}
		}
	}

	// Flush remaining content
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
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
