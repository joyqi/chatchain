package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/muesli/termenv"
)

// BackgroundReporter is the optional capability for hosts that know the
// terminal background WITHOUT a terminal round-trip (a multiplexer with a
// theme API). Such a channel is safe mid-Program, which the ANSI OSC 11
// query is not — it would race the event loop's stdin ownership. ok=false
// means "don't know"; resolution falls through.
type BackgroundReporter interface {
	DarkBackground() (dark bool, ok bool)
}

// backgroundProbes mirror the detector registry for startup background
// resolution, most specific first. Probes are plain functions (not host
// constructions): resolving a color must not spin up exec workers.
var backgroundProbes = []func(Env) (dark bool, ok bool){cmuxBackgroundProbe}

// DetectBackground resolves the terminal background at startup: host-native
// probes first, then the ANSI fallback's OSC 11 round-trip. The fallback
// talks to the tty, so call this only before the Program claims stdin.
func DetectBackground(env Env) bool {
	for _, probe := range backgroundProbes {
		if dark, ok := probe(env); ok {
			return dark
		}
	}
	return terminalDarkBackground()
}

// DarkBackground asks the first detected host that knows its background
// without a terminal round-trip (per capability, safe between turns).
// ok=false: no host knows — the startup answer stands.
func (p *Presenter) DarkBackground() (bool, bool) {
	for _, h := range p.hosts {
		if br, ok := h.(BackgroundReporter); ok {
			if dark, known := br.DarkBackground(); known {
				return dark, true
			}
		}
	}
	return false, false
}

// bgUnsupported latches when the terminal ignored the OSC query, so repeat
// calls don't pay termenv's timeout again.
var bgUnsupported bool

// queryTerminalDark is the tty round-trip (OSC 11 via termenv, which also
// consults COLORFGBG); a seam for tests.
var queryTerminalDark = termenv.HasDarkBackground

// terminalDarkBackground is the ANSI fallback: one OSC 11 query, dark when
// the terminal doesn't say (termenv's default — most terminals are dark).
func terminalDarkBackground() bool {
	if bgUnsupported {
		return true
	}
	start := time.Now()
	dark := queryTerminalDark()
	if time.Since(start) > time.Second {
		bgUnsupported = true
	}
	return dark
}

// cmuxBackgroundProbe resolves the pane background through cmux's RPC —
// terminal_background rides every terminal.replay reply (verified live:
// #FEFFFF on a light theme, ~30ms). No tty involved.
func cmuxBackgroundProbe(env Env) (bool, bool) {
	sid := env.Getenv("CMUX_SURFACE_ID")
	if sid == "" {
		return false, false
	}
	path, err := env.LookPath("cmux")
	if err != nil {
		return false, false
	}
	return cmuxBackground(path, sid)
}

func cmuxBackground(path, sid string) (bool, bool) {
	out, err := cmuxQuery(path, sid)
	if err != nil {
		return false, false
	}
	return parseCmuxBackground(out)
}

// cmuxQuery runs the replay RPC; a seam for tests. Bounded hard: this runs
// synchronously at turn start, and a wedged cmux must not stall the chat.
var cmuxQuery = func(path, sid string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	params, _ := json.Marshal(map[string]string{"terminal_id": sid})
	return exec.CommandContext(ctx, path, "rpc", "terminal.replay", string(params)).Output()
}

func parseCmuxBackground(out []byte) (bool, bool) {
	var v struct {
		RenderGrid struct {
			TerminalBackground string `json:"terminal_background"`
		} `json:"render_grid"`
	}
	if json.Unmarshal(out, &v) != nil {
		return false, false
	}
	return darkHex(v.RenderGrid.TerminalBackground)
}

// darkHex classifies a #RRGGBB color as dark by perceived luminance.
func darkHex(s string) (dark, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return false, false
	}
	var r, g, b int
	if _, err := fmt.Sscanf(s[1:], "%02x%02x%02x", &r, &g, &b); err != nil {
		return false, false
	}
	return (299*r+587*g+114*b)/1000 < 128, true
}
