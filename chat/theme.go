package chat

import (
	"time"

	"chatchain/internal/markdown"
	"chatchain/internal/ui"

	"github.com/muesli/termenv"
)

// bgUnsupported latches when the terminal ignores the OSC 11 background query,
// so later turns don't pay termenv's timeout again.
var bgUnsupported bool

// detectCodeTheme re-detects the terminal background (OSC 11 via termenv) and
// updates the markdown code theme. Call it only at quiet moments (startup, the
// start of a turn) — never mid-stream — so the query can't race user
// keystrokes. A responsive terminal answers in milliseconds, so per-turn
// re-detection is cheap and tracks light/dark switches; a terminal that
// ignores OSC 11 hits termenv's 5s timeout once, after which we latch off.
//
// This is a TERMINAL interaction, which is why it lives in chat (the caller)
// rather than in the pure internal/markdown package; the result is injected
// via markdown.SetCodeTheme.
func detectCodeTheme() {
	if bgUnsupported {
		return
	}
	start := time.Now()
	dark := termenv.HasDarkBackground()
	if time.Since(start) > time.Second {
		bgUnsupported = true
	}
	ui.SetDarkBackground(dark) // adaptive ui shades (input background)
	if dark {
		markdown.SetCodeTheme("monokai")
	} else {
		markdown.SetCodeTheme("github")
	}
}
