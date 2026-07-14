package ui

import (
	"unicode/utf8"

	"chatchain/internal/textwidth"
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
