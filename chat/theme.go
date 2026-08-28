package chat

import (
	"github.com/joyqi/iota/internal/host"
	"github.com/joyqi/iota/internal/markdown"
	"github.com/joyqi/iota/internal/ui"
)

// detectCodeTheme resolves the terminal background at startup through the
// host layer (internal/host): a host with a native theme channel (cmux RPC)
// answers without touching the tty; the plain terminal pays one OSC 11
// round-trip, which is why this must run pre-Program — mid-Program it would
// race the event loop's stdin ownership.
func detectCodeTheme() {
	applyCodeTheme(host.DetectBackground(host.SystemEnv()))
}

// refreshCodeTheme re-resolves the background between turns through hosts
// that answer without a terminal round-trip (the Presenter capability) —
// tracking a light/dark switch mid-session. Everywhere else ok=false and
// the startup theme stands. Runs on the chat goroutine at turn start, so
// the theme never flips inside a streaming block.
func refreshCodeTheme(pres *host.Presenter) {
	if dark, ok := pres.DarkBackground(); ok {
		applyCodeTheme(dark)
	}
}

// applyCodeTheme fans the resolved background out to every shade consumer:
// the markdown code theme, the adaptive ui shades (input background), and
// the showcase diff renderer's block shades (diff.go). Already-printed code
// keeps its baked-in colors — only new blocks pick up a change.
func applyCodeTheme(dark bool) {
	ui.SetDarkBackground(dark)
	themeDark = dark
	if dark {
		markdown.SetCodeTheme("monokai")
		diffCodeTheme = "monokai"
	} else {
		markdown.SetCodeTheme("github")
		diffCodeTheme = "github"
	}
}

// themeDark / diffCodeTheme mirror the detected background for the showcase
// diff renderer: the 256-color ± block shades and the chroma style must both
// track light/dark, and the markdown package keeps its own copy private.
var (
	themeDark     = true
	diffCodeTheme = "monokai"
)
