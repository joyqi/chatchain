package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/joyqi/iota/internal/textwidth"
)

// clipLine returns the slice of s covering visible columns [start,
// start+width), preserving ANSI escapes and dropping any wide rune that
// straddles a boundary (the v1 promptui viewer implementation, ported for the
// View panel's horizontal pan).
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
		rw := textwidth.RuneWidth(r)
		if col >= start && col+rw <= start+width {
			b = append(b, s[i:i+size]...)
		}
		col += rw
		i += size
	}
	return string(b)
}

// wrapANSI hard-wraps a possibly styled line into rows of at most width
// columns (ANSI- and CJK-aware via ansi.Hardwrap), then re-emits the SGR
// state active at each row boundary so every row renders correctly in
// isolation: a viewport clipped mid-line must not lose the line's styling
// when the row that opened it scrolls out of view.
func wrapANSI(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	rows := strings.Split(ansi.Hardwrap(s, width, true), "\n")
	state := ""
	for i, row := range rows {
		if i > 0 && state != "" {
			rows[i] = state + row
		}
		state = sgrCarry(state, row)
	}
	return rows
}

// sgrCarry folds row's SGR sequences into the accumulated open-state string
// (a single merged SGR sequence, or "" when reset). Parameter order is
// preserved, so extended sequences like 38;2;r;g;b survive intact.
func sgrCarry(state, row string) string {
	for i := 0; i < len(row); {
		n := ansiLen(row, i)
		if n == 0 {
			_, size := utf8.DecodeRuneInString(row[i:])
			i += size
			continue
		}
		seq := row[i : i+n]
		i += n
		if seq[len(seq)-1] != 'm' { // CSI but not SGR
			continue
		}
		params := seq[2 : len(seq)-1]
		if params == "" || params == "0" {
			state = ""
			continue
		}
		// A leading reset clears the state before the rest applies ("0;7").
		if rest, ok := strings.CutPrefix(params, "0;"); ok {
			state = ""
			params = rest
		}
		if state == "" {
			state = "\x1b[" + params + "m"
		} else {
			state = state[:len(state)-1] + ";" + params + "m"
		}
	}
	return state
}

// ansiLen returns the byte length of the ANSI CSI escape starting at s[i], or
// 0 if there isn't one.
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
