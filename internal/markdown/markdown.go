package markdown

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"

	"chatchain/internal/mathtext"
	"chatchain/internal/textwidth"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/list"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/fatih/color"
	"github.com/muesli/termenv"

	chromastyles "github.com/alecthomas/chroma/v2/styles"
)

// mdRenderer is the single lipgloss renderer used by the markdown path.
//
// Color-profile rule: fatih/color (used app-wide) emits 16-color SGR gated
// solely by the global color.NoColor flag, while lipgloss by default binds a
// renderer to its output and silently strips ANSI when that output is not a
// terminal — which would desync the two stacks and break tests that render
// into a bytes.Buffer and assert on escape codes. We therefore never let
// lipgloss auto-detect: syncMDRenderer pins the profile to ANSI (16-color)
// when color.NoColor is false and Ascii when it is true, so both stacks
// always toggle together. The renderer's writer is irrelevant (io.Discard) —
// it only exists so profile detection is never consulted.
var mdRenderer = lipgloss.NewRenderer(io.Discard)

// syncMDRenderer aligns mdRenderer's color profile with the global
// color.NoColor flag (see mdRenderer). Called before each lipgloss render
// because tests and CLI flags flip color.NoColor at runtime.
func syncMDRenderer() {
	if color.NoColor {
		mdRenderer.SetColorProfile(termenv.Ascii)
	} else {
		mdRenderer.SetColorProfile(termenv.ANSI)
	}
}

// Markdown-path text styles, all bound to mdRenderer so they obey the
// color-profile rule above. They mirror the fatih/color styles in styles.go
// (which the rest of the app keeps) with the same 16-color SGR output;
// H1 additionally gets an underline.
// TabWidth(NoTabConversion) keeps lipgloss from rewriting tabs inside the
// text — this writer styles lines without altering their content.
var (
	mdPlain  = mdRenderer.NewStyle().TabWidth(lipgloss.NoTabConversion)
	mdBold   = mdPlain.Bold(true)                                      // **bold**, table headers
	mdItalic = mdPlain.Italic(true)                                    // *italic*
	mdCode   = mdPlain.Foreground(lipgloss.Color("6"))                 // `code`: cyan, as CodeStyle
	mdLink   = mdPlain.Foreground(lipgloss.Color("6")).Underline(true) // link text, as LinkStyle
	mdDim    = mdPlain.Faint(true)                                     // quote bars, bullets, rules, URLs
	mdH1     = mdBold.Underline(true)                                  // # heading
	mdH2     = mdBold                                                  // ## and deeper: plain bold
	// mdQuote frames a blockquote block: a continuous left bar (│) drawn by
	// lipgloss's border on every row (so it stays connected across wrapped and
	// blank rows, unlike a per-line glyph), tinted the same cyan as code so it
	// ties into the palette, with one column of padding before the text. The
	// quote text itself keeps its normal foreground and inline styling — the
	// border owns the left column, so there is no faint span to cut. Under
	// color.NoColor lipgloss still draws the bar glyph but drops its color.
	mdQuote = mdRenderer.NewStyle().
		TabWidth(lipgloss.NoTabConversion).
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color("6")).
		PaddingLeft(1)
)

// quoteBorderCols is the terminal columns the quote frame adds around its text:
// the left border glyph (1) plus PaddingLeft (1). The lipgloss style Width
// includes padding but not the border, so the content width passed to Width is
// termWidth-1 and text wraps at termWidth-quoteBorderCols.
const quoteBorderCols = 2

// Hyperlink wraps text in an OSC 8 terminal hyperlink. Terminals without
// support ignore the sequence and show the plain text — progressive
// enhancement, no capability sniffing. The URL is stripped of control bytes
// so crafted content can never terminate or escape the OSC sequence (the
// window-title lesson). Under color.NoColor (piped output) the text passes
// through bare — escape sequences don't belong in pipes.
func Hyperlink(url, text string) string {
	if color.NoColor {
		return text
	}
	url = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, url)
	return termenv.Hyperlink(url, text)
}

// headingStyle maps a heading level to its style: H1 bold+underline, every
// other level plain bold. (Bold+faint "level differentiation" for H3+ was
// tried and reverted: ###/#### are the levels models emit most, and terminals
// render the 1;2 combination faint-dominant — whole documents looked washed
// out.)
func headingStyle(level int) lipgloss.Style {
	if level == 1 {
		return mdH1
	}
	return mdH2
}

// mdUnit classifies the last thing the writer emitted so the routing helpers
// (emitBlank/emitText/beginBlock/endBlock) can guarantee exactly one blank line
// around block-level elements while never splitting a paragraph.
type mdUnit int

const (
	unitNone  mdUnit = iota // nothing emitted yet (suppresses leading blanks)
	unitBlank               // the last emitted unit was a blank line
	unitText                // the last emitted unit was a paragraph line
	unitBlock               // the last emitted unit was a rendered block element
)

// Sink is where the Writer sends output and reads terminal context. It is
// the purity seam: the renderer itself never touches the terminal.
//
//   - Write receives committed rendered lines (ANSI text).
//   - Width is consulted LIVE at each block flush, so a mid-stream terminal
//     resize changes the layout of the NEXT block (today's behavior).
//   - BlockPreview opens a live "rendering…" preview for a buffering block
//     (table/code/list/quote/math); the returned writer receives the RAW
//     source lines and Close clears the preview. It may return nil — the
//     piped/non-TTY shape, where blocks render with no live preview.
type Sink interface {
	io.Writer
	Width() int
	BlockPreview(label string) io.WriteCloser
}

// plainSink is the no-preview Sink over a plain writer at a fixed width: the
// shape used for piped output, tests, and blockquote child rendering.
type plainSink struct {
	w     io.Writer
	width int
}

func (s plainSink) Write(p []byte) (int, error)        { return s.w.Write(p) }
func (s plainSink) Width() int                         { return s.width }
func (s plainSink) BlockPreview(string) io.WriteCloser { return nil }

// NewWriterTo returns a Writer emitting plain rendered output to w at a fixed
// layout width — no live previews (the piped/non-TTY shape).
func NewWriterTo(w io.Writer, width int) *Writer { return NewWriter(plainSink{w: w, width: width}) }

// Writer applies ANSI highlighting to markdown syntax elements line by line,
// without modifying the original text content, emitting through a Sink.
type Writer struct {
	w         Sink
	buf       []byte
	inFence   bool
	tableRows [][]string // buffered parsed cells per row
	tableSeps []bool     // true if row is a separator (|---|---|)
	fenceLang string     // language from the opening ``` fence
	codeLines []string   // buffered code-block lines, highlighted at the close
	inList    bool       // a list block is buffering
	listItems []listItem // parsed items of the buffering list block
	listLoose bool       // a blank line was kept inside the block (loose list)
	// gapPaid: the buffering block's separating blank was already written when
	// its preview opened, so beginBlock must not write it again (openPreview).
	gapPaid   bool
	listBlank bool     // one blank line is held, pending the next line's verdict
	inQuote   bool     // a blockquote block is buffering
	quoteBody []string // buffered inner quote lines (one leading "> "/">" stripped)
	inMath    bool     // a display-math ($$…$$ / \[…\]) block is buffering
	mathLines []string // buffered inner display-math lines (fence lines excluded)
	lastUnit  mdUnit   // classification of the last emitted unit (spacing state machine)
	// tableView / codeView / listView / quoteView show a live "rendering…"
	// preview of a block while it buffers (nil when the sink has no preview);
	// the flush Closes it before emitting the result.
	tableView io.WriteCloser
	codeView  io.WriteCloser
	listView  io.WriteCloser
	quoteView io.WriteCloser
	mathView  io.WriteCloser
}

// NewWriter returns a Writer emitting through sink.
func NewWriter(w Sink) *Writer {
	// lastUnit starts unitNone so leading blank lines (before any content) are
	// suppressed and no separator is inserted before the first block or text.
	return &Writer{w: w, lastUnit: unitNone}
}

func (m *Writer) Write(p []byte) (int, error) {
	m.buf = append(m.buf, p...)

	for {
		idx := indexOf(m.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(m.buf[:idx])
		m.buf = m.buf[idx+1:]
		if err := m.consumeLine(line); err != nil {
			return len(p), err
		}
	}

	return len(p), nil
}

// consumeLine dispatches one complete line through the block state machine.
// It is the single home of every "does this line belong to the open block"
// decision: Write feeds it terminated lines, Flush feeds it the final partial
// line — so the two paths can never disagree on block affinity.
func (m *Writer) consumeLine(line string) error {
	// Fenced code block: buffer until the closing fence, then highlight once.
	if strings.HasPrefix(strings.TrimSpace(line), "```") {
		if m.inFence {
			m.inFence = false
			if err := m.flushCode(); err != nil {
				return err
			}
		} else {
			if len(m.tableRows) > 0 {
				if err := m.flushTable(); err != nil {
					return err
				}
			}
			// A fence inside a list is out of scope: flush the list first.
			if m.inList {
				if err := m.finishList(); err != nil {
					return err
				}
			}
			// A fence ends any open quote block, mirroring the list case.
			if m.inQuote {
				if err := m.flushQuote(); err != nil {
					return err
				}
			}
			m.inFence = true
			m.fenceLang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
			m.codeLines = nil
			m.codeView = m.openPreview(codeLabel(m.fenceLang))
		}
		return nil
	}
	if m.inFence {
		m.codeLines = append(m.codeLines, line)
		if m.codeView != nil {
			io.WriteString(m.codeView, line+"\n")
		}
		return nil
	}

	// A buffering list block consumes marker lines, indented continuation
	// text, and one held blank line; anything else flushes the block and
	// falls through to be processed normally.
	if m.inList {
		consumed, err := m.listConsume(line)
		if err != nil {
			return err
		}
		if consumed {
			return nil
		}
	}

	// A buffering quote block consumes consecutive quote lines; the first
	// non-quote line flushes it and falls through to be processed normally.
	if m.inQuote {
		if isQuoteLine(line) {
			m.quoteAppend(line)
			return nil
		}
		if err := m.flushQuote(); err != nil {
			return err
		}
	}

	// A buffering display-math block consumes lines until its closing fence
	// ("$$" or "\]"), then renders the 2D layout. Placed after the quote
	// check and before the table check, mirroring the fenced-code block.
	if m.inMath {
		if mathtext.IsDisplayClose(line) {
			if err := m.flushMath(); err != nil {
				return err
			}
		} else {
			m.mathAppend(line)
		}
		return nil
	}

	// Open a display-math block: a bare "$$"/"\[" fence starts a buffered
	// multi-line block, while a complete one-line "$$…$$"/"\[…\]" renders at
	// once. Either form first flushes a pending table/list (a display formula
	// is its own block unit); the quote path already flushed above.
	if body, oneLine, ok := mathtext.DisplayOpen(line); ok {
		if len(m.tableRows) > 0 {
			if err := m.flushTable(); err != nil {
				return err
			}
		}
		if m.inList {
			if err := m.finishList(); err != nil {
				return err
			}
		}
		if oneLine {
			m.mathLines = []string{body}
			if err := m.flushMath(); err != nil {
				return err
			}
		} else {
			m.startMath()
		}
		return nil
	}

	if isTableLine(line) {
		m.tableConsume(line)
		return nil
	}

	if len(m.tableRows) > 0 {
		if err := m.flushTable(); err != nil {
			return err
		}
	}

	if isListLine(line) {
		m.startList(line)
		return nil
	}

	if isQuoteLine(line) {
		m.startQuote(line)
		return nil
	}

	// Plain path: classify the line for the spacing state machine. A blank
	// collapses; a heading or horizontal rule is a block-level element
	// bounded by one blank line above and below; anything else is a
	// paragraph line that stays adjacent to its neighbours.
	switch {
	case strings.TrimSpace(line) == "":
		if err := m.emitBlank(); err != nil {
			return err
		}
	case isBlockLine(line):
		if err := m.beginBlock(); err != nil {
			return err
		}
		if _, err := io.WriteString(m.w, m.highlightLine(line)+"\n"); err != nil {
			return err
		}
		m.endBlock()
	default:
		if err := m.emitText(m.highlightLine(line)); err != nil {
			return err
		}
	}
	return nil
}

// Flush renders whatever is still buffered at end of stream. The trailing
// partial line is final by definition, so it runs through consumeLine exactly
// like a terminated line — Flush holds no affinity logic of its own (a
// hand-maintained copy drifted for years: tables dropped their last row,
// headings/math/list markers in a final partial line printed raw). Any block
// left open is then closed: fence/math/list/quote are mutually exclusive
// (opening one flushes the others), and a table that survived dispatch closes
// after them.
func (m *Writer) Flush() {
	if len(m.buf) > 0 {
		line := string(m.buf)
		m.buf = nil
		m.consumeLine(line)
	}
	switch {
	case m.inFence:
		m.inFence = false
		m.flushCode()
	case m.inMath:
		m.flushMath()
	case m.inList:
		m.finishList()
	case m.inQuote:
		m.flushQuote()
	}
	if len(m.tableRows) > 0 {
		m.flushTable()
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
func (m *Writer) highlightLine(line string) string {
	syncMDRenderer()
	trimmed := strings.TrimSpace(line)

	// Heading: ## Title → drop the # markers, style the text by level.
	if len(trimmed) > 0 && trimmed[0] == '#' {
		i := 0
		for i < len(trimmed) && trimmed[i] == '#' {
			i++
		}
		if i < len(trimmed) && trimmed[i] == ' ' {
			// Inline markers inside the heading style on top of the heading's
			// own look: renderInline composes attributes downward (heading
			// bold/underline + code cyan), so no inner reset can cut the
			// heading mid-line — the flaw that used to force stripping.
			return renderInline(strings.TrimSpace(trimmed[i:]), headingStyle(i), true)
		}
	}

	// Horizontal rule: --- or *** or ___
	if isHorizontalRule(trimmed) {
		return mdDim.Render(line)
	}

	// Blockquotes are rendered as a buffered block by the Write loop
	// (startQuote/flushQuote), so they never reach highlightLine.

	// List item: normalize the bullet (- * + → •) and dim it, style the rest.
	if marker, rest, ok := splitListMarker(line); ok {
		return renderListMarker(marker) + highlightInline(rest)
	}

	// Regular line: apply inline highlighting
	return highlightInline(line)
}

// isHeadingLine reports whether the line is an ATX heading (one or more '#'
// markers followed by a space and text) — the same shape highlightLine styles
// with headingStyle.
func isHeadingLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '#' {
		return false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	return i < len(trimmed) && trimmed[i] == ' '
}

// isBlockLine reports whether a plain-path line is a block-level element that
// must be bounded by one blank line above and below: a heading or a horizontal
// rule. (Tables, lists, quotes, and fences are handled by their own buffered
// paths and never reach here.)
func isBlockLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return isHeadingLine(line) || isHorizontalRule(trimmed)
}

// highlightInline applies inline markdown styling and hides the markup itself:
// **bold**/__bold__ → bold text, *italic*/_italic_ → italic text, `code` →
// styled text (no backticks), and [text](url) → styled text + a dim URL.
func highlightInline(line string) string {
	syncMDRenderer()
	return renderInline(line, mdPlain, false)
}

// renderInline styles one line's inline constructs with STYLE-CONTEXT
// PASSING: container constructs (bold, italic, link text) recurse with their
// attribute composed onto base, and every atomic segment renders with the
// full composed style, self-contained. Nesting escape sequences instead
// (outer bold wrapping an inner styled span) would let the inner reset cut
// the outer style mid-line — the reason nesting was unsupported before.
// Leaves stay literal: a code span's content is never re-parsed (`*args*`
// keeps its asterisks), and precedence is scan order — code before math
// before emphasis, ** before *. styled says whether base carries any
// attributes; a plain run under a plain base is emitted verbatim, keeping
// top-level output byte-identical to the pre-nesting renderer.
func renderInline(line string, base lipgloss.Style, styled bool) string {
	var out strings.Builder
	var plain strings.Builder
	flush := func() {
		if plain.Len() == 0 {
			return
		}
		if styled {
			out.WriteString(base.Render(plain.String()))
		} else {
			out.WriteString(plain.String())
		}
		plain.Reset()
	}
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
					flush()
					out.WriteString(Hyperlink(url, renderInline(text, base.Foreground(lipgloss.Color("6")).Underline(true), true)))
					out.WriteString(base.Faint(true).Render(" (" + url + ")"))
					i = urlEnd + 1
					continue
				}
			}
		}

		// Inline code: `code` → styled, backticks hidden.
		//
		// This branch runs BEFORE the inline-math branch below so a code span
		// wins: "`$x$`" stays literal code, never approximated math.
		if runes[i] == '`' {
			if end := findClose(runes, i+1, '`'); end > 0 {
				flush()
				out.WriteString(base.Foreground(lipgloss.Color("6")).Render(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}

		// Escaped dollar: "\$" is a literal "$", never a math opener. Consume the
		// backslash and emit the bare "$" so it can neither open a span here nor
		// close one that a later "$" might try to form. Only "\$" is unescaped —
		// every other backslash passes through untouched (prose is not LaTeX).
		if runes[i] == '\\' && i+1 < len(runes) && runes[i+1] == '$' {
			plain.WriteByte('$')
			i += 2
			continue
		}

		// Inline math: $...$ or \(...\) → a single-line Unicode approximation,
		// delimiters hidden. Detection rule (see findInlineMath):
		//   - "\$" is a literal dollar (backslash-escaped), never an opener.
		//   - A "$" is math only when a matching UNESCAPED "$" closes it later on
		//     the same line AND the enclosed body is non-empty; otherwise the "$"
		//     is emitted literally. A closing "$" immediately followed by a digit
		//     is currency, not a close, so "$5 or $10" and "$20,000" stay literal
		//     while a formula like "$1/\pi$" (digit opener, non-digit close) forms.
		//   - "\(" ... "\)" is always math (an explicit LaTeX delimiter).
		// ApproxInline guarantees a single physical line, so inline math is safe
		// inside table cells (styledCell → highlightInline) — no row-height
		// surprise. Styling reuses mdCode (cyan), the same treatment as inline
		// code: math is a verbatim technical token, so it reads as a peer of code
		// in the palette and honors color.NoColor via the md* lipgloss styles.
		// A "$$" run is a display fence (P2), never inline math: emit both
		// dollars atomically and skip past them. Advancing by only one would
		// leave the second "$" to be re-scanned as a valid inline opener on the
		// next iteration, which would eat a single-line "$$x$$" into "$x$".
		if runes[i] == '$' && i+1 < len(runes) && runes[i+1] == '$' {
			plain.WriteString("$$")
			i += 2
			continue
		}
		if body, end, ok := findInlineMath(runes, i); ok {
			flush()
			out.WriteString(base.Foreground(lipgloss.Color("6")).Render(mathtext.ApproxInline(body)))
			i = end
			continue
		}

		// Bold italic: ***text*** → both attributes composed. Without this
		// branch the ** pair would match greedily inside the *** run and
		// orphan the third star ("*x" bold plus a stray "*").
		if i+2 < len(runes) && runes[i] == '*' && runes[i+1] == '*' && runes[i+2] == '*' {
			if end := findTripleClose(runes, i+3); end > 0 {
				flush()
				out.WriteString(renderInline(string(runes[i+3:end]), base.Bold(true).Italic(true), true))
				i = end + 3
				continue
			}
		}

		// Bold: **text** / __text__ → bold, markers hidden.
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			if end := findDoubleClose(runes, i+2, '*'); end > 0 {
				flush()
				out.WriteString(renderInline(string(runes[i+2:end]), base.Bold(true), true))
				i = end + 2
				continue
			}
		}
		if i+1 < len(runes) && runes[i] == '_' && runes[i+1] == '_' {
			if end := findDoubleClose(runes, i+2, '_'); end > 0 {
				flush()
				out.WriteString(renderInline(string(runes[i+2:end]), base.Bold(true), true))
				i = end + 2
				continue
			}
		}

		// Italic: *text* / _text_ → italic, markers hidden.
		// Avoid matching list bullets and horizontal rules.
		if runes[i] == '*' && i+1 < len(runes) && runes[i+1] != '*' && runes[i+1] != ' ' {
			if end := findClose(runes, i+1, '*'); end > i+1 {
				flush()
				out.WriteString(renderInline(string(runes[i+1:end]), base.Italic(true), true))
				i = end + 1
				continue
			}
		}
		if runes[i] == '_' && i+1 < len(runes) && runes[i+1] != '_' && runes[i+1] != ' ' {
			// Only match if preceded by space or start of line.
			if i == 0 || unicode.IsSpace(runes[i-1]) {
				if end := findClose(runes, i+1, '_'); end > i+1 {
					flush()
					out.WriteString(renderInline(string(runes[i+1:end]), base.Italic(true), true))
					i = end + 1
					continue
				}
			}
		}

		plain.WriteRune(runes[i])
		i++
	}
	flush()

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
// findTripleClose finds the first "***" run at or after start.
func findTripleClose(runes []rune, start int) int {
	for i := start; i < len(runes)-2; i++ {
		if runes[i] == '*' && runes[i+1] == '*' && runes[i+2] == '*' {
			return i
		}
	}
	return -1
}

func findDoubleClose(runes []rune, start int, delim rune) int {
	for i := start; i < len(runes)-1; i++ {
		if runes[i] == delim && runes[i+1] == delim {
			return i
		}
	}
	return -1
}

// findInlineMath reports whether an inline math span opens at runes[start] and,
// if so, returns its inner body and the rune index just past the closing
// delimiter. It is the rune-index twin of internal/mathtext.FindInline (the
// markdown inline loop indexes runes, not bytes), applying the same rules:
//
//   - "$ … $": a math span only when the opening "$" is not immediately
//     followed by a space ("$ 5"), an UNESCAPED "$" closes it later on this
//     line, and the body between the two dollars is non-empty. A digit right
//     after the opener is fine (so "$1/\pi$" renders); the currency case is
//     ruled out at the CLOSE — a "$" immediately followed by a digit is not a
//     close, which keeps "it costs $5 or $10" and "$20,000" literal. A lone
//     "$" with no close stays literal too.
//   - "\$" is a backslash-escaped literal dollar and never opens a span; the
//     opener check skips it because runes[start] is the '\', not the '$'.
//   - "\( … \)": always math (an explicit LaTeX inline delimiter).
//
// A "$$" at start is a display fence (P2), not inline, and is rejected here.
func findInlineMath(runes []rune, start int) (body string, end int, ok bool) {
	switch runes[start] {
	case '$':
		if start+1 < len(runes) && runes[start+1] == '$' {
			return "", 0, false // "$$" display fence, handled in P2
		}
		// Opener guard: a "$" at end of line or immediately followed by a space
		// ("$ 5") is not a math opener. A digit right after it IS allowed —
		// "$1/\pi$" is a real formula — so the currency case is handled at the
		// close (a "$" followed by a digit does not close). This mirrors
		// internal/mathtext (isCurrencyDollar + scanDollarMath) so the markdown
		// path and the package agree on what a "$" span is.
		if start+1 >= len(runes) {
			return "", 0, false // trailing lone "$"
		}
		if runes[start+1] == ' ' {
			return "", 0, false // "$ " — spacing/currency, never a math opener
		}
		// Scan for the matching unescaped "$" on this line (highlightInline is
		// already called per line, so runes never span a newline).
		for i := start + 1; i < len(runes); i++ {
			if runes[i] == '\\' {
				i++ // skip the escaped rune so "\$" cannot close the span
				continue
			}
			if runes[i] == '$' {
				// A "$" immediately followed by a digit is currency ("$10"),
				// not a close — keep scanning. This keeps "$5 or $10" literal
				// now that a digit right after the opener is allowed (so
				// "$1/\pi$" renders).
				if i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
					continue
				}
				if runes[i-1] == ' ' {
					continue // space before the close ("$a and $b"): the second
					// "$" opens a new token, it does not close the first.
				}
				inner := string(runes[start+1 : i])
				if strings.TrimSpace(inner) == "" {
					return "", 0, false // empty body ("$ $"): not math
				}
				return inner, i + 1, true
			}
		}
		return "", 0, false // no closing "$": the "$" is literal (lone/currency)
	case '\\':
		// Explicit "\( … \)" inline math.
		if start+1 < len(runes) && runes[start+1] == '(' {
			for i := start + 2; i < len(runes); i++ {
				if runes[i] == '\\' {
					if i+1 < len(runes) && runes[i+1] == ')' {
						return string(runes[start+2 : i]), i + 2, true
					}
					i++ // skip any other escape
					continue
				}
			}
		}
		return "", 0, false
	}
	return "", 0, false
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
// preserved so nested lists stay aligned. This is the fallback for stray list
// lines that bypass the buffered block path (e.g. a partial line at Flush);
// whole list blocks are rendered by flushList.
func renderListMarker(marker string) string {
	bullet := strings.TrimLeft(marker, " \t")
	indent := marker[:len(marker)-len(bullet)]
	switch {
	case strings.HasPrefix(bullet, "- "), strings.HasPrefix(bullet, "* "), strings.HasPrefix(bullet, "+ "):
		return indent + mdDim.Render("• ")
	default:
		return indent + mdDim.Render(bullet)
	}
}

// listItem is one parsed item of a buffering list block.
type listItem struct {
	level  int      // nesting depth, derived from source indentation
	marker string   // "•", "☐", "☑", or the ordered token as written ("3.", "7)")
	lines  []string // item text: first line + continuations ("" = paragraph break)
}

// taskMarkerRe matches a task-list checkbox at the start of an item's text.
var taskMarkerRe = regexp.MustCompile(`^\[([ xX])\](?: |$)`)

// isListLine reports whether the line opens a list item. Horizontal rules like
// "- - -" also match the marker regex, so they are excluded explicitly.
func isListLine(line string) bool {
	if isHorizontalRule(strings.TrimSpace(line)) {
		return false
	}
	return listMarkerRe.MatchString(line)
}

// indentLevel converts a list marker's leading whitespace into a nesting
// level: every two columns are one level (a tab counts as two columns), so
// two-space, three-space, and tab-indented nested bullets all land on the
// level their author intended.
func indentLevel(indent string) int {
	cols := 0
	for _, r := range indent {
		if r == '\t' {
			cols += 2
		} else {
			cols++
		}
	}
	return cols / 2
}

// startList opens a list block with the given marker line as its first item.
func (m *Writer) startList(line string) {
	m.inList = true
	m.listView = m.openPreview("rendering list…")
	m.listAppendItem(line)
	m.listPreview(line)
}

// listConsume feeds one line to the buffering list block. It reports whether
// the line was consumed; when it was not, the block (and any held blank line)
// has already been flushed and the caller must process the line normally.
func (m *Writer) listConsume(line string) (bool, error) {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		if m.listBlank {
			// A second blank line ends the list; both blanks re-emit after it
			// (the held one here, the current one via the caller).
			return false, m.finishList()
		}
		m.listBlank = true
		m.listPreview(line)
		return true, nil
	case isListLine(line):
		if m.listBlank {
			// The held blank separated two items: the list is loose.
			m.listBlank = false
			m.listLoose = true
		}
		m.listAppendItem(line)
		m.listPreview(line)
		return true, nil
	case line[0] == ' ' || line[0] == '\t':
		// An indented table under a list item is still a table (LLMs commonly
		// nest one below a bullet): flush the list and let the caller's table
		// branch take the line — the same courtesy the fence branch extends.
		if isTableLine(trimmed) {
			return false, m.finishList()
		}
		// Indented text continues the previous item.
		it := &m.listItems[len(m.listItems)-1]
		if m.listBlank {
			// The held blank split the item into paragraphs: keep the break
			// inside the item and treat the list as loose.
			m.listBlank = false
			m.listLoose = true
			it.lines = append(it.lines, "")
		}
		it.lines = append(it.lines, trimmed)
		m.listPreview(line)
		return true, nil
	default:
		return false, m.finishList()
	}
}

// listAppendItem parses a marker line into a new item of the buffering block.
func (m *Writer) listAppendItem(line string) {
	marker, rest, _ := splitListMarker(line)
	bullet := strings.TrimLeft(marker, " \t")
	level := indentLevel(marker[:len(marker)-len(bullet)])
	if n := len(m.listItems); n == 0 {
		level = 0 // a block always starts at the top level
	} else if prev := m.listItems[n-1].level; level > prev+1 {
		level = prev + 1 // never skip a level, whatever the source indentation
	}
	glyph := "•"
	if bullet[0] >= '0' && bullet[0] <= '9' {
		glyph = strings.TrimSpace(bullet) // ordered: keep the number as written
	} else if t := taskMarkerRe.FindString(rest); t != "" {
		if strings.ContainsAny(t, "xX") {
			glyph = "☑"
		} else {
			glyph = "☐"
		}
		rest = rest[len(t):]
	}
	m.listItems = append(m.listItems, listItem{level: level, marker: glyph, lines: []string{rest}})
}

// listPreview mirrors a consumed raw line into the live block preview.
func (m *Writer) listPreview(line string) {
	if m.listView != nil {
		io.WriteString(m.listView, line+"\n")
	}
}

// finishList flushes the buffering list block and re-emits a held blank line
// after it (the blank turned out to end the list, not to make it loose).
func (m *Writer) finishList() error {
	err := m.flushList()
	if m.listBlank {
		m.listBlank = false
		// Route the held blank through emitBlank so it participates in the
		// blank-run collapse. flushList set lastUnit=unitBlock, so this genuine
		// separator survives; whatever follows already gets exactly one blank
		// from its own boundary, so this never doubles up.
		if werr := m.emitBlank(); err == nil {
			err = werr
		}
	}
	return err
}

// flushList clears the live preview and renders the buffered list block.
func (m *Writer) flushList() error {
	if m.listView != nil {
		m.listView.Close() // clear the live preview before emitting the list
		m.listView = nil
	}
	items := m.listItems
	loose := m.listLoose
	m.listItems = nil
	m.listLoose = false
	m.inList = false
	if len(items) == 0 {
		return nil
	}
	if err := m.beginBlock(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(m.w, renderList(items, loose))
	m.endBlock() // the rendered list is a block unit
	return err
}

// isQuoteLine reports whether the line opens or continues a blockquote: its
// trimmed form starts with "> " or is exactly ">".
func isQuoteLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == ">" || strings.HasPrefix(trimmed, "> ")
}

// stripQuoteMarker removes exactly one leading "> " (or a bare ">") from a
// quote line, returning the inner content (empty for a bare ">").
func stripQuoteMarker(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == ">" {
		return ""
	}
	return strings.TrimPrefix(trimmed, "> ")
}

// startQuote opens a blockquote block with the given line as its first inner
// line.
func (m *Writer) startQuote(line string) {
	m.inQuote = true
	m.quoteBody = nil
	m.quoteView = m.openPreview("rendering quote…")
	m.quoteAppend(line)
}

// quoteAppend buffers one quote line's inner content and mirrors the raw line
// into the live preview.
func (m *Writer) quoteAppend(line string) {
	m.quoteBody = append(m.quoteBody, stripQuoteMarker(line))
	if m.quoteView != nil {
		io.WriteString(m.quoteView, line+"\n")
	}
}

// flushQuote clears the live preview and renders the buffered blockquote as a
// single lipgloss block so the left bar stays continuous down every row. The
// flush counts as non-blank output for the blank-run collapse.
func (m *Writer) flushQuote() error {
	if m.quoteView != nil {
		m.quoteView.Close() // clear the live preview before emitting the quote
		m.quoteView = nil
	}
	body := m.quoteBody
	m.quoteBody = nil
	m.inQuote = false
	if len(body) == 0 {
		return nil
	}
	if err := m.beginBlock(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(m.w, m.renderQuote(body))
	m.endBlock() // the rendered quote is a block unit
	return err
}

// termWidth returns the width for block layout: the sink's live width (so a
// mid-stream resize changes the next block), 80 when the sink reports none.
func (m *Writer) termWidth() int {
	if tw := m.w.Width(); tw > 0 {
		return tw
	}
	return 80
}

// renderQuote frames the buffered inner content in the mdQuote block style with
// a continuous left bar. The inner lines are a mini-document: they are rendered
// recursively through a child Writer (so lists, headings, tables,
// nested quotes, and inline markdown inside a quote all work by reusing the
// full pipeline), at a width reduced by the quote frame so nested blocks fit.
// The bar is then prepended to every line of that pre-rendered content — no
// Width is set on the style, so lipgloss never re-wraps the child's tables.
func (m *Writer) renderQuote(body []string) string {
	syncMDRenderer()

	inner := m.termWidth() - quoteBorderCols
	if inner < quoteBorderCols+1 {
		inner = quoteBorderCols + 1
	}
	var buf strings.Builder
	child := NewWriter(plainSink{w: &buf, width: inner})
	_, _ = child.Write([]byte(strings.Join(body, "\n") + "\n"))
	child.Flush()

	content := strings.TrimRight(buf.String(), "\n")
	// Pin the style width so lipgloss draws the bar on EVERY visual row: a long
	// plain paragraph line (the child leaves paragraphs unwrapped, relying on
	// the terminal) is soft-wrapped here to the content width, and each wrapped
	// row gets the bar. Width is the content area (padding included, border
	// excluded) = termWidth-1; text then wraps at termWidth-quoteBorderCols =
	// inner. The child already laid out tables/code to <= inner, so those lines
	// never exceed Width and lipgloss leaves them intact — only overlong
	// paragraphs wrap.
	width := m.termWidth() - 1
	if width < quoteBorderCols+1 {
		width = quoteBorderCols + 1
	}
	return mdQuote.Width(width).Render(content)
}

// emitBlank writes a blank separator line, collapsing runs of blanks and
// suppressing leading blanks: it does nothing when the last unit was already a
// blank (or nothing has been emitted yet), otherwise it writes one "\n".
func (m *Writer) emitBlank() error {
	if m.lastUnit == unitBlank || m.lastUnit == unitNone {
		return nil // collapse a run of blanks (and drop leading blanks)
	}
	m.lastUnit = unitBlank
	_, err := io.WriteString(m.w, "\n")
	return err
}

// emitText writes one already-styled paragraph line. If the previous unit was a
// block, one separating blank line is inserted first (block→paragraph
// boundary); after a text unit no separator is written so consecutive plain
// lines stay in the same paragraph.
func (m *Writer) emitText(s string) error {
	if m.lastUnit == unitBlock {
		if _, err := io.WriteString(m.w, "\n"); err != nil {
			return err
		}
	}
	m.lastUnit = unitText
	_, err := io.WriteString(m.w, s+"\n")
	return err
}

// beginBlock is called right before a block-level element renders its content.
// It inserts one separating blank line when the previous unit was text or a
// block (text→block or block→block boundary); after a blank or at the start it
// writes nothing. It deliberately does not update lastUnit — the block's render
// followed by endBlock does that.
//
// A buffering block whose preview already paid the separator (openPreview)
// consumes that credit here instead of writing a second blank.
func (m *Writer) beginBlock() error {
	if m.gapPaid {
		m.gapPaid = false
		return nil
	}
	if m.needsGap() {
		_, err := io.WriteString(m.w, "\n")
		return err
	}
	return nil
}

// needsGap reports whether a block starting now must be preceded by a
// separating blank line.
func (m *Writer) needsGap() bool { return m.lastUnit == unitText || m.lastUnit == unitBlock }

// openPreview opens a buffering block's live preview, paying its separating
// blank line FIRST — the preview occupies the row the rendered block will, so
// it has to sit where that block will sit. Paying at block OPEN (rather than
// at flush, where beginBlock would) is what keeps the streaming layout and the
// settled layout identical; beginBlock then skips the blank it already owes.
//
// Without this the gap only appeared when the source happened to carry a blank
// line before the block — which markdown does not require, and which models
// routinely omit ("Here is a list:" followed straight by the bullets).
func (m *Writer) openPreview(label string) io.WriteCloser {
	if m.needsGap() {
		io.WriteString(m.w, "\n")
		m.gapPaid = true
	}
	return m.w.BlockPreview(label)
}

// endBlock records that a rendered block was just written. The blank line that
// follows a block is produced lazily by the next unit's separator, so nothing
// is emitted here — this only updates the state machine.
func (m *Writer) endBlock() {
	m.lastUnit = unitBlock
}

// renderList lays out a parsed list block with lipgloss/list. Each top-level
// item (with its nested descendants) is rendered as its own list and the
// blocks are joined with a blank line in between when the list is loose —
// lipgloss/list has no notion of inter-item spacing, so looseness is this
// thin manual layer on top. Enumerator alignment across the blocks is kept by
// padding every top-level marker to the same width first.
func renderList(items []listItem, loose bool) string {
	syncMDRenderer()
	// EnumeratorStyle replaces lipgloss's default PaddingRight(1), so the dim
	// marker style must bring its own.
	enumStyle := mdDim.PaddingRight(1)

	topW := 0
	for _, it := range items {
		if it.level == 0 {
			if w := displayWidth(it.marker); w > topW {
				topW = w
			}
		}
	}

	var blocks []string
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].level > 0 {
			end++
		}
		blocks = append(blocks, buildList(items[start:end], enumStyle, topW).String())
		start = end
	}

	sep := "\n"
	if loose {
		sep = "\n\n"
	}
	return strings.Join(blocks, sep)
}

// buildList assembles one lipgloss list for a run of items whose first entry
// sets the base level; deeper runs become nested sublists attached to the
// item before them. Markers are the items' own (bullets, task glyphs, ordered
// numbers as written — lipgloss's stock enumerators renumber from 1, so a
// closure serves each item its recorded marker instead), left-padded to a
// common width of at least minWidth so ordered numbers right-align. The
// indenter matches that width, giving continuation lines and sublists a
// hanging indent aligned with the item text.
func buildList(items []listItem, enumStyle lipgloss.Style, minWidth int) *list.List {
	base := items[0].level
	l := list.New()
	var markers []string
	for i := 0; i < len(items); {
		if items[i].level > base {
			j := i
			for j < len(items) && items[j].level > base {
				j++
			}
			// A nested *list.List merges into the item right before it.
			l.Item(buildList(items[i:j], enumStyle, 0))
			i = j
			continue
		}
		l.Item(listItemText(items[i]))
		markers = append(markers, items[i].marker)
		i++
	}

	w := minWidth
	for _, s := range markers {
		if dw := displayWidth(s); dw > w {
			w = dw
		}
	}
	for i, s := range markers {
		if pad := w - displayWidth(s); pad > 0 {
			markers[i] = strings.Repeat(" ", pad) + s
		}
	}

	return l.
		Enumerator(func(_ list.Items, i int) string {
			if i < len(markers) {
				return markers[i]
			}
			return "•"
		}).
		EnumeratorStyle(enumStyle).
		Indenter(func(list.Items, int) string { return strings.Repeat(" ", w) })
}

// listItemText renders an item's buffered lines with inline markdown styling;
// continuation lines join with newlines so lipgloss/list hangs them under the
// first line.
func listItemText(it listItem) string {
	lines := make([]string, len(it.lines))
	for i, ln := range it.lines {
		lines[i] = highlightInline(ln)
	}
	return strings.Join(lines, "\n")
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
		// Tabs are normalized to a single space at the parse boundary so every
		// downstream ruler agrees: uniseg measures \t as 0 columns, lipgloss's
		// style renderer would expand it to 4 spaces, and the table resizer's
		// height estimate treats it as 1 — the disagreement made cell content
		// after a tab wrap invisibly and get clipped by the row height. A
		// literal tab inside a bordered row would also misalign on real
		// terminals (tab stops), so a plain space is the safe rendering.
		cells[i] = strings.TrimSpace(stripVariationSelectors(strings.ReplaceAll(p, "\t", " ")))
	}
	return cells
}

// stripVariationSelectors removes the emoji/text presentation selectors
// (U+FE0F/U+FE0E) from table cells so bordered layouts stay aligned on every
// terminal. No two parties agree on a VS16 sequence's width: uniseg (and
// iTerm2, kitty) say "\u2696\ufe0f" is 2 columns while Terminal.app advances the
// cursor by the base rune's width (1) and lets the glyph overflow — so any
// padding computed for such a sequence misaligns somewhere. The bare base
// rune has one consistent answer everywhere (narrow 1, wide 2), at the
// cosmetic cost of a monochrome glyph on some terminals. Applied only where
// content is padded to measured widths (table cells); free-flowing text keeps
// its selectors.
//
// Flag emoji (regional-indicator pairs) and ZWJ/skin-tone sequences share the
// same cross-terminal ambiguity but are deliberately left untouched: they
// have no lossless narrow form (replacing flags with their country codes was
// tried and rejected — the flags matter more than the borders), so tables
// containing them may misalign on terminals whose cursor advance disagrees
// with uniseg. That is accepted.
func stripVariationSelectors(s string) string {
	if !strings.ContainsRune(s, '\uFE0F') && !strings.ContainsRune(s, '\uFE0E') {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\uFE0F' || r == '\uFE0E' {
			return -1
		}
		return r
	}, s)
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

// newBlockPreview opens a live rolling preview of a block's raw lines as they
// stream in, but only when writing to a terminal — off a terminal (pipe, tests)
// there is no cursor control, so the raw lines would just duplicate the rendered
// result. Returns nil when there is no terminal.
func codeLabel(lang string) string {
	if lang == "" {
		return "rendering code…"
	}
	return "rendering code (" + lang + ")…"
}

// flushCode clears the live preview and emits the buffered code block, syntax
// highlighted for the terminal.
func (m *Writer) flushCode() error {
	if m.codeView != nil {
		m.codeView.Close()
		m.codeView = nil
	}
	code := strings.Join(m.codeLines, "\n")
	lang := m.fenceLang
	m.codeLines = nil
	m.fenceLang = ""
	if err := m.beginBlock(); err != nil {
		return err
	}
	_, err := io.WriteString(m.w, highlightCode(code, lang))
	m.endBlock() // the rendered code block is a block unit
	return err
}

// startMath opens a buffered display-math block. Its inner lines are collected
// until the closing fence, then laid out in 2D by flushMath.
func (m *Writer) startMath() {
	m.inMath = true
	m.mathLines = nil
	m.mathView = m.openPreview("rendering math…")
}

// mathAppend buffers one inner display-math line and mirrors the raw line into
// the live preview.
func (m *Writer) mathAppend(line string) {
	m.mathLines = append(m.mathLines, line)
	if m.mathView != nil {
		io.WriteString(m.mathView, line+"\n")
	}
}

// mathIndent is the uniform left margin added to every rendered display-math
// row, matching the code block's indent so formulas and code sit on the same
// left rule.
const mathIndent = "  "

// flushMath clears the live preview and emits the buffered display-math block as
// a 2D layout (mathtext.Render2D). It mirrors flushCode: beginBlock/endBlock make
// the formula a block unit with one blank line above and below, and it rides the
// quote child writer's width override so math inside a blockquote fits inside the
// bar. When the formula cannot be parsed/laid out, Render2D returns the cleaned
// single-line Unicode approximation (the same ApproxInline the inline `$…$` path
// uses); either way the result is plain math text.
//
// The fallback is rendered in NORMAL color, exactly like the 2D layout — it is
// still the reader's formula, and dimming legible content only hurts readability
// (dim is reserved for decoration: quote bars, bullets, rules, URLs). Both
// outcomes are glyph-based and carry no ANSI of their own, so under
// color.NoColor the whole block stays escape-free.
func (m *Writer) flushMath() error {
	if m.mathView != nil {
		m.mathView.Close() // clear the live preview before emitting the block
		m.mathView = nil
	}
	lines := m.mathLines
	m.mathLines = nil
	m.inMath = false

	src := strings.Join(lines, "\n")
	if strings.TrimSpace(src) == "" {
		return nil // an empty $$ block renders nothing (mirrors flushQuote)
	}
	if err := m.beginBlock(); err != nil {
		return err
	}
	// A full 2D layout (ok=true) or the single-line fallback (ok=false) both
	// render the same way: plain, per-row indented, undimmed.
	block, _ := mathtext.Render2D(src, m.termWidth()-len(mathIndent))
	_, err := io.WriteString(m.w, indentMath(block))
	m.endBlock() // the rendered math block is a block unit
	return err
}

// indentMath prefixes every row of a 2D layout with mathIndent. The layout is
// plain (no ANSI), so the indent never inherits a style.
func indentMath(s string) string {
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = mathIndent + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

// codeIndent is the uniform left margin added to every rendered code-block line,
// matching the buffering preview's indent.
const codeIndent = "  "

// highlightCode syntax-highlights a code block to ANSI via chroma, falling back
// to a plain block if highlighting fails (e.g. unknown content). Every line is
// indented by codeIndent. When color.NoColor is set, chroma is bypassed so the
// block carries no escape codes — the same switch that silences the lipgloss
// and fatih/color styles (see mdRenderer).
func highlightCode(code, lang string) string {
	if color.NoColor {
		return indentCode(code)
	}
	// A bare fence has nothing to highlight: the plaintext lexer marks every
	// rune Text, so chroma would paint the whole block in the style's plain
	// foreground — betting the block's readability on background detection
	// (monokai's Text is near-white: invisible on a light terminal a
	// misdetection left it on). The terminal's default foreground is readable
	// by definition; github already behaves this way (it defines no Text
	// color), so this only changes the dark-theme path.
	if lang == "" {
		return indentCode(code)
	}
	var sb strings.Builder
	if err := Highlight(&sb, code, lang, codeStyleName()); err != nil {
		return indentCode(codeFallbackStyle.Sprint(code))
	}
	return indentCode(sb.String())
}

// Highlight syntax-highlights code to ANSI via chroma (terminal256), with
// one crucial deviation from stock chroma: ERROR tokens render as plain
// text. Lexers mark anything they cannot parse as Error — CJK punctuation,
// prose inside template literals, dialect syntax — and most styles paint
// Error with an alarm BACKGROUND (github: white on deep red, monokai: dark
// red), which showed up as garish blocks scattered through otherwise normal
// code. A parse failure deserves no styling at all. This is also what keeps
// the "chroma emits only foreground colors" invariant true — the diff
// renderer's background blocks rely on it. Exported for the chat layer's
// diff highlighting, so both paths share the exact pipeline.
func Highlight(w io.Writer, code, lang, style string) error {
	l := lexers.Get(lang)
	if l == nil {
		l = lexers.Fallback
	}
	it, err := chroma.Coalesce(l).Tokenise(nil, code)
	if err != nil {
		return err
	}
	tokens := it.Tokens()
	for i := range tokens {
		if tokens[i].Type == chroma.Error || tokens[i].Type.Category() == chroma.Error {
			tokens[i].Type = chroma.Text
		}
	}
	f := formatters.Get("terminal256")
	if f == nil {
		f = formatters.Fallback
	}
	s := chromastyles.Get(style)
	if s == nil {
		s = chromastyles.Fallback
	}
	return f.Format(w, s, chroma.Literator(tokens...))
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

// codeTheme is the chroma style matching the current terminal background.
// The OSC background query that picks it is a terminal interaction and lives
// with the caller (chat/ui); it injects the result via SetCodeTheme.
var codeTheme = "monokai"

// SetCodeTheme sets the chroma style used for subsequent code blocks
// ("monokai" for dark backgrounds, "github" for light).
func SetCodeTheme(name string) { codeTheme = name }

// codeFallbackStyle colors a code block when chroma highlighting fails
// (matching the app-wide green used for code).
var codeFallbackStyle = color.New(color.FgGreen)

// codeStyleName returns the chroma style for the current terminal background. A
// light background gets a light theme so dark-on-light text stays readable;
// otherwise a dark theme. Highlight guarantees the output carries only
// foreground colors (Error tokens neutralized), so no background block is
// painted either way.
//
// Already-printed code keeps its baked-in colors — only new blocks pick up a
// background change.
func codeStyleName() string { return codeTheme }

// flushTable renders the buffered table rows with aligned columns via
// lipgloss/table. If the table would exceed terminal width, columns are
// shrunk (water-filling) and cell text wraps within the cell across multiple
// visual lines.
// tableConsume buffers one table row — parsed cells plus its separator flag —
// opening the live preview on the first row and feeding it the raw line.
func (m *Writer) tableConsume(line string) {
	if len(m.tableRows) == 0 {
		m.tableView = m.openPreview("rendering table…")
	}
	cells := parseTableCells(line)
	m.tableRows = append(m.tableRows, cells)
	m.tableSeps = append(m.tableSeps, isTableSeparator(cells))
	if m.tableView != nil {
		io.WriteString(m.tableView, line+"\n")
	}
}

func (m *Writer) flushTable() error {
	if m.tableView != nil {
		m.tableView.Close() // clear the live preview before emitting the table
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
	// proportionally shrink so the table fits the terminal; lipgloss wraps each
	// cell to these maxima (ANSI- and grapheme-aware) and handles
	// alignment/borders.
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
	tw := m.termWidth()
	overhead := 1 + maxCols*3 // leading border + " cell " + border per column
	// Target width-1, never the exact terminal width: an exact-width row
	// sits on the terminal's deferred-wrap boundary, and the ui's overflow
	// sanitizer trims one column off exact-multiple rows — which ate the
	// table's right border. Same width-1 convention as the ui's own wrapping.
	available := tw - 1 - overhead
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

	syncMDRenderer()
	baseCell := mdRenderer.NewStyle().Padding(0, 1)
	tbl := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(mdRenderer.NewStyle().Faint(true)).
		BorderRow(true). // a ├─┼─┤ rule between every pair of rows
		StyleFunc(func(_, col int) lipgloss.Style {
			if col < 0 || col >= maxCols {
				return baseCell
			}
			// A style Width pins the whole column: lipgloss pads short cells
			// and wraps long ones to it. +2 covers the one-space padding on
			// each side of the cell content.
			return baseCell.Width(colWidths[col] + 2)
		})

	// The header goes in as an ordinary first row rather than via Headers():
	// lipgloss/table clamps header cells to a single line (MaxHeight(1) +
	// truncation), which would break multi-line <br> headers. With
	// BorderRow(true) the rule under the header is drawn either way.
	for i, row := range rows {
		if seps[i] {
			continue // markdown |---| row; BorderRow draws the rules instead
		}
		cells := make([]string, maxCols)
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
		tbl.Row(cells...)
	}

	if err := m.beginBlock(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(m.w, tbl.Render())
	m.endBlock() // the rendered table is a block unit
	return err
}

// styledCell renders a data cell's inline markdown (markers hidden) and turns
// <br> tags into newlines so lipgloss lays them out as multi-line cells.
func styledCell(cell string) string {
	segs := splitBR(cell)
	for i, s := range segs {
		segs[i] = highlightInline(strings.TrimSpace(s))
	}
	return strings.Join(segs, "\n")
}

// headerCell strips a header cell's inline markers and bolds each line. Each
// segment is bolded on its own so the bold never spans a newline (which would
// otherwise bleed into the table borders).
func headerCell(cell string) string {
	segs := splitBR(cell)
	for i, s := range segs {
		segs[i] = mdBold.Render(stripInlineMarkdown(strings.TrimSpace(s)))
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

// displayWidth returns the terminal display width of a string. It measures
// uniseg grapheme clusters, which is sequence-aware: a VS16 emoji such as
// "⚖️" (U+2696 U+FE0F) counts as 2 columns — matching how terminals render
// it — and combining marks and ZWJ sequences collapse into their cluster.
// This is the same measurement lipgloss uses, so tables built from these
// widths stay aligned. Use it for any whole string; see runeWidth for the
// single-rune seam.
func displayWidth(s string) int {
	return textwidth.StringWidth(s)
}

// runeWidth is the single-rune twin (see textwidth.RuneWidth for the ruler
// rationale).
func runeWidth(r rune) int {
	return textwidth.RuneWidth(r)
}
