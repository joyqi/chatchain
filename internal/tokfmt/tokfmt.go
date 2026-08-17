// Package tokfmt renders token counts for the UI. One implementation so every
// figure — the status line's ↑/↓/↺ and window, the /status panel, the thinking
// marker's token meter, the context-window picker — reads the same, the way
// internal/timefmt owns elapsed durations. Three copies of these nine lines
// had drifted apart into separate spellings before this.
package tokfmt

import (
	"strconv"
	"strings"
)

// Tokens renders a count compactly with k/m units and one decimal, trailing
// ".0" trimmed: 842→"842", 1234→"1.2k", 128000→"128k", 1500000→"1.5m".
// Counts below 1000 stay exact — at that size the digits are the information.
func Tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "m"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "k"
	default:
		return strconv.Itoa(n)
	}
}

func trimZero(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}
