package chat

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"
	"github.com/fatih/color"

	"chatchain/internal/markdown"
)

// The showcase diff renderer: unified hunks become an annotated listing —
// a dim line-number gutter on the left (the @@ headers are translated away;
// nobody should have to read "@@ -0,0 +1,15 @@"), syntax-highlighted code,
// and background blocks marking additions and deletions the way Claude
// Code's diffs do. Context rows stay dim; hunk boundaries show as a "⋮" row.

// diffRow is one display row parsed from unified hunk lines: the marker
// kind, the line number it carries (new-file numbering for additions and
// context, old-file for deletions), and the code text without the marker.
// A gap row separates hunks.
type diffRow struct {
	kind rune // '+', '-', ' '
	num  int
	gap  bool
	text string
}

// parseDiffRows walks unified hunk lines, translating @@ headers into
// running line counters. "\ No newline at end of file" markers are display
// noise and drop; anything unrecognized counts as context so a foreign
// artifact degrades instead of derailing. Tabs expand to spaces — the
// padding and truncation math needs real display widths.
func parseDiffRows(lines []string) []diffRow {
	var rows []diffRow
	oldN, newN := 1, 1
	seenHunk := false
	text := func(s string) string { return strings.ReplaceAll(s, "\t", "    ") }
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "@@"):
			o, n, ok := parseHunkHeader(ln)
			if ok {
				oldN, newN = o, n
			}
			if seenHunk {
				rows = append(rows, diffRow{gap: true})
			}
			seenHunk = true
		case strings.HasPrefix(ln, "+"):
			rows = append(rows, diffRow{kind: '+', num: newN, text: text(ln[1:])})
			newN++
		case strings.HasPrefix(ln, "-"):
			rows = append(rows, diffRow{kind: '-', num: oldN, text: text(ln[1:])})
			oldN++
		case strings.HasPrefix(ln, "\\"):
			// "\ No newline at end of file"
		default:
			rows = append(rows, diffRow{kind: ' ', num: newN, text: text(strings.TrimPrefix(ln, " "))})
			oldN++
			newN++
		}
	}
	return rows
}

// parseHunkHeader extracts the old and new start lines from an
// "@@ -a[,b] +c[,d] @@ …" header. A zero start (a fresh file's "-0,0")
// normalizes to 1 — the first emitted row IS line 1 of its side.
func parseHunkHeader(ln string) (oldStart, newStart int, ok bool) {
	fields := strings.Fields(ln)
	if len(fields) < 3 {
		return 0, 0, false
	}
	o, oerr := strconv.Atoi(strings.SplitN(strings.TrimPrefix(fields[1], "-"), ",", 2)[0])
	n, nerr := strconv.Atoi(strings.SplitN(strings.TrimPrefix(fields[2], "+"), ",", 2)[0])
	if oerr != nil || nerr != nil {
		return 0, 0, false
	}
	if o < 1 {
		o = 1
	}
	if n < 1 {
		n = 1
	}
	return o, n, true
}

// diffBG returns the 256-color background sequences for added and deleted
// rows, matching the detected terminal background.
func diffBG() (add, del string) {
	if themeDark {
		return "\x1b[48;5;22m", "\x1b[48;5;52m" // deep green / deep red
	}
	return "\x1b[48;5;194m", "\x1b[48;5;224m" // pale green / pale red
}

// diffLexer names the chroma lexer for the artifact's file path ("" = no
// highlighting — plain text inside the blocks).
func diffLexer(title string) string {
	if title == "" {
		return ""
	}
	l := lexers.Match(filepath.Base(title))
	if l == nil {
		return ""
	}
	return l.Config().Name
}

// highlightDiffLine syntax-highlights one code line and re-arms the given
// background after every chroma reset, so the block color survives token
// styling. markdown.Highlight neutralizes Error tokens — a lexer choking on
// out-of-context line fragments must not splash alarm backgrounds through
// the block. Chroma works line-at-a-time here; constructs spanning lines
// may shade differently than a whole-file pass — acceptable for a diff.
func highlightDiffLine(text, lang, bg string) string {
	if lang != "" {
		var sb strings.Builder
		if err := markdown.Highlight(&sb, text, lang, diffCodeTheme); err == nil {
			h := strings.TrimRight(sb.String(), "\n")
			return strings.ReplaceAll(h, "\x1b[0m", "\x1b[0m"+bg)
		}
	}
	return text
}

// renderDiff renders unified hunk lines for the scrollback. Rows beyond the
// budget collapse into a "… +N more lines" tail, and overwide code TRUNCATES
// (wrapping would wreck the alignment). Width ≤ 3 (startup, tests) skips
// width handling; with color disabled the listing is plain "NNN + code".
func renderDiff(title string, lines []string, budget, width int) []string {
	rows := parseDiffRows(lines)
	shown, extra := rows, 0
	if len(rows) > budget {
		shown = rows[:budget-1]
		extra = len(rows) - len(shown)
	}

	maxN := 1
	for _, r := range shown {
		if r.num > maxN {
			maxN = r.num
		}
	}
	gw := len(strconv.Itoa(maxN))
	// "  " indent + gutter + space + "+ " marker; the code gets the rest.
	codeW := width - 2 - gw - 3
	lang := diffLexer(title)
	bgAdd, bgDel := diffBG()

	out := make([]string, 0, len(shown)+1)
	for _, r := range shown {
		if r.gap {
			out = append(out, "  "+dim(strings.Repeat(" ", gw)+" ⋮"))
			continue
		}
		text := r.text
		if width > 3 && codeW > 8 && ansi.StringWidth(text) > codeW {
			text = ansi.Truncate(text, codeW-1, "…")
		}
		gutter := DimStyle.Sprintf("%*d", gw, r.num)
		var body string
		switch {
		case color.NoColor:
			body = string(r.kind) + " " + text
		case r.kind == ' ':
			body = DimStyle.Sprint("  " + text)
		default:
			bg := bgAdd
			if r.kind == '-' {
				bg = bgDel
			}
			pad := ""
			if codeW > 8 {
				if n := codeW - ansi.StringWidth(text); n > 0 {
					pad = strings.Repeat(" ", n)
				}
			}
			body = bg + string(r.kind) + " " + highlightDiffLine(text, lang, bg) + pad + "\x1b[0m"
		}
		out = append(out, "  "+gutter+" "+body)
	}
	if extra > 0 {
		out = append(out, dim(fmt.Sprintf("  … +%d more lines", extra)))
	}
	return out
}
