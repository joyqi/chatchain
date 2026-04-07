package chat

import (
	"io"
	"regexp"
	"strings"
	"unicode"
)

// markdownWriter wraps an io.Writer and applies ANSI highlighting to markdown
// syntax elements line by line, without modifying the original text content.
type markdownWriter struct {
	w       io.Writer
	buf     []byte
	inFence bool
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

		highlighted := m.highlightLine(line)
		if _, err := io.WriteString(m.w, highlighted+"\n"); err != nil {
			return len(p), err
		}
	}

	return len(p), nil
}

// Flush writes any remaining buffered content.
func (m *markdownWriter) Flush() {
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
