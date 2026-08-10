package ui

import "sync/atomic"

// This file is the single home of every color the frame and its surfaces
// emit. The ui layer deliberately renders with raw SGR fragments (no lipgloss
// styling in the frame): whatever the palette does must survive being
// clipped, wrapped, and re-emitted by the staging window and wrapANSI, and
// bare fragments compose predictably there. Semantics:
//
//	faint  — chrome and secondary text: separators, hints, placeholders
//	cyan   — the accent: cursor rows, markers, prompt, spinner
//	green  — positive: checked boxes, command highlight
//	yellow — warning: a figure heading somewhere the user should notice
//	red    — alerts (ErrPrefix)
//	revOn  — strong emphasis: focused tab chips, user blocks
//
// Chat-side TEXT colors live in chat/styles.go (fatih/color, NoColor-aware);
// markdown/code rendering owns its own theme in internal/markdown (chroma +
// lipgloss, converging at P6). One home per layer.
const (
	faint    = "\x1b[2m"
	cyan     = "\x1b[36m"
	green    = "\x1b[32m"
	yellow   = "\x1b[33m"
	red      = "\x1b[31m"
	revOn    = "\x1b[7m"
	sgrReset = "\x1b[0m"
)

// ErrPrefix styles surface-level error rows.
const ErrPrefix = red + "⚠ " + sgrReset

// darkBackground tracks the detected terminal background (true until told
// otherwise — dark is the safer default for the adaptive shades below).
var darkBackground atomic.Bool

func init() { darkBackground.Store(true) }

// SetDarkBackground records the terminal's background tone (from the chat
// layer's OSC background query) so adaptive shades pick the right side.
func SetDarkBackground(dark bool) { darkBackground.Store(dark) }

// inputBg is the input field's background: one gray step off the terminal
// background — distinguishable, not loud. Text renders in the default
// foreground on top of it; the placeholder adds faint.
func inputBg() string {
	if darkBackground.Load() {
		return "\x1b[48;5;236m"
	}
	return "\x1b[48;5;254m"
}
