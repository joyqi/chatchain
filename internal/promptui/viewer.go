package promptui

import (
	"io"
	"os"
	"unicode/utf8"

	"chatchain/internal/promptui/screenbuf"
	"chatchain/internal/readline"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// Viewer is a read-only pager for multi-line text. It scrolls vertically,
// pages (←→ / space / Ctrl+F/B), pans horizontally (h/l), jumps to top/bottom
// (g/G), and quits on q / Esc / Ctrl+C. Unlike Select it makes no selection — it
// just displays content that may exceed the screen. Lines may contain ANSI
// styling; both horizontal clipping and wrapping preserve the escape codes so
// colors stay intact.
type Viewer struct {
	// Label is an optional bold title line shown above the content.
	Label string
	// Lines are the content lines to display (may contain ANSI styling). Each
	// element is one logical line; they must not contain \r or \n.
	Lines []string
	// Wrap soft-wraps each logical line to the terminal width across multiple
	// display rows instead of clipping it; horizontal panning is then disabled.
	Wrap bool
	// Height is the number of visible content rows. 0 fits the terminal.
	Height int

	Stdin  io.ReadCloser
	Stdout io.WriteCloser
}

// Run displays the viewer and blocks until the user quits.
func (v *Viewer) Run() error {
	width, termHeight := 80, 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width, termHeight = w, h
	}

	// maxRows is what the terminal can show (leaving room for the label, help,
	// and the readline line). Height defaults to filling it, but is capped at it
	// so an explicit Height never overflows a short terminal — and capped at the
	// content length so we never pad past the end.
	maxRows := termHeight - 3
	if maxRows < 1 {
		maxRows = 1
	}
	// Display lines: soft-wrap each logical line to the width when Wrap is set,
	// otherwise show them as-is (and pan horizontally).
	lines := v.Lines
	if v.Wrap {
		var wrapped []string
		for _, ln := range v.Lines {
			wrapped = append(wrapped, wrapLine(ln, width)...)
		}
		lines = wrapped
	}

	height := v.Height
	if height <= 0 || height > maxRows {
		height = maxRows
	}
	if height > len(lines) {
		height = len(lines)
	}
	if height < 1 {
		height = 1
	}

	maxLineWidth := 0
	if !v.Wrap {
		for _, ln := range lines {
			if w := visibleWidth(ln); w > maxLineWidth {
				maxLineWidth = w
			}
		}
	}

	c := &readline.Config{
		Stdin:          v.Stdin,
		Stdout:         v.Stdout,
		UniqueEditLine: true, // we own the screen; readline only reads keys
		EscapeCancels:  true, // ESC quits
	}
	if err := c.Init(); err != nil {
		return err
	}
	rl, err := readline.NewEx(c)
	if err != nil {
		return err
	}
	rl.Write([]byte(hideCursor))
	sb := screenbuf.New(rl)

	vOff, hOff := 0, 0
	maxV := func() int {
		if m := len(lines) - height; m > 0 {
			return m
		}
		return 0
	}
	clamp := func() {
		if vOff > maxV() {
			vOff = maxV()
		}
		if vOff < 0 {
			vOff = 0
		}
		if hOff > maxLineWidth {
			hOff = maxLineWidth
		}
		if hOff < 0 {
			hOff = 0
		}
	}

	bold := Styler(FGBold)
	helpText := "↑↓ scroll · ←→/space page · h/l pan · g/G top/bottom · q quit"
	if v.Wrap {
		helpText = "↑↓ scroll · ←→/space page · g/G top/bottom · q quit"
	}
	help := Styler(FGFaint)(helpText)

	redraw := func() {
		if v.Label != "" {
			sb.WriteString(bold(v.Label))
		}
		for i := 0; i < height; i++ {
			if idx := vOff + i; idx < len(lines) {
				sb.WriteString(clipLine(lines[idx], hOff, width))
			} else {
				sb.WriteString("")
			}
		}
		sb.WriteString(help)
		sb.Flush()
	}

	c.FuncFilterInputRune = func(r rune) (rune, bool) {
		if r == 'q' {
			return readline.CharInterrupt, true
		}
		return r, true
	}

	c.SetListener(func(line []rune, pos int, key rune) ([]rune, int, bool) {
		switch {
		case key == KeyPrev || key == 'k':
			vOff-- // up one line
		case key == KeyNext || key == 'j':
			vOff++ // down one line
		case key == KeyForward || key == ' ':
			vOff += height // page down (→ / Ctrl+F / space)
		case key == KeyBackward || key == 'b':
			vOff -= height // page up (← / Ctrl+B / b)
		case key == 'h':
			hOff-- // pan left one column
		case key == 'l':
			hOff++ // pan right one column
		case key == 'g':
			vOff, hOff = 0, 0
		case key == 'G':
			vOff = maxV()
		}
		clamp()
		redraw()
		return nil, 0, true
	})

	redraw() // initial frame

	for {
		if _, err := rl.Readline(); err != nil {
			break // ESC / q / Ctrl+C / EOF
		}
	}

	clearScreen(sb)
	rl.Write([]byte(showCursor))
	rl.Close()
	return nil
}

// visibleWidth returns the display width of s in terminal columns, skipping
// ANSI CSI escape sequences (e.g. color codes). Double-width runes (CJK,
// emoji) count as 2 columns.
func visibleWidth(s string) int {
	col := 0
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		col += runewidth.RuneWidth(r)
		i += size
	}
	return col
}

// clipLine returns the slice of s covering visible columns [start, start+width),
// counting double-width runes (CJK, emoji) as two columns. A wide rune that
// straddles either edge of the window is dropped rather than shown halved.
// ANSI CSI escape sequences are always emitted (regardless of position) so the
// color state carries into the visible window and resets are preserved.
func clipLine(s string, start, width int) string {
	if width <= 0 {
		return ""
	}
	var b []byte
	col := 0
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			b = append(b, s[i:i+n]...)
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		if col >= start && col+rw <= start+width {
			b = append(b, s[i:i+size]...)
		}
		col += rw
		i += size
	}
	return string(b)
}

// ansiLen returns the byte length of the ANSI CSI escape sequence starting at
// s[i] (ESC '[' … final-byte), or 0 if there isn't one there.
func ansiLen(s string, i int) int {
	if i+1 >= len(s) || s[i] != '\x1b' || s[i+1] != '[' {
		return 0
	}
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j < len(s) {
		j++ // include the final byte
	}
	return j - i
}

// wrapLine soft-wraps s into rows of at most width visible columns, ANSI- and
// width-aware: double-width runes (CJK, emoji) count as two columns, and the
// row is flushed before a wide rune that would overflow it, so no row ever
// exceeds width. The active SGR style is tracked across rows: each row
// re-establishes it at the start and closes it at the end, so colors stay
// correct after a wrap. A reset (\x1b[0m / \x1b[m) clears the tracked style.
func wrapLine(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var rows []string
	active := ""
	cur := make([]byte, 0, len(s))
	col := 0
	flush := func() {
		row := string(cur)
		if active != "" {
			row += "\x1b[0m"
		}
		rows = append(rows, row)
		cur = append(cur[:0], active...)
		col = 0
	}
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			code := s[i : i+n]
			cur = append(cur, code...)
			if code == "\x1b[0m" || code == "\x1b[m" {
				active = ""
			} else {
				active += code
			}
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		// Flush before an overflowing wide rune; col > 0 guards against a
		// zero-progress loop when a single rune is wider than the whole row.
		if col > 0 && col+rw > width {
			flush()
		}
		cur = append(cur, s[i:i+size]...)
		col += rw
		i += size
		if col >= width {
			flush()
		}
	}
	if col > 0 || len(rows) == 0 {
		flush()
	}
	return rows
}
