package host

import (
	"context"
	"io"
	"os/exec"
	"time"
)

// Cmux drives the cmux sidebar (github.com/manaflow-ai/cmux): the status
// pill with icon/color via `cmux set-status`, and the workspace loading
// spinner via `cmux workspace loading` — the same public CLI surface cmux's
// own agent integrations use, giving display parity with Claude Code.
//
// Detection: cmux injects CMUX_SURFACE_ID into every pane's environment and
// puts its CLI on PATH there.
//
// Cmux deliberately does NOT implement Notifier: the ANSI host's OSC 9
// notification passes through cmux's ingress already (verified live) and
// keeps our focus gating; taking the channel over would hand presence
// policy to cmux's untested side. State updates are best-effort decoration —
// a single worker executes them off the chat loop, a last-wins mailbox
// coalesces bursts, failures are silent, and nothing here may slow a turn.
type Cmux struct {
	exec func(argv []string) // one CLI invocation; tests inject a recorder
	mail chan [][]string     // capacity 1; post replaces a waiting batch
	done chan struct{}
	// background resolves the pane background through cmux's RPC (see
	// background.go) — the BackgroundReporter capability: tty-free, so the
	// code theme can track light/dark switches BETWEEN turns, which the
	// ANSI OSC path cannot do once the Program owns stdin.
	background func() (dark, ok bool)
}

// cmuxKey names both the status row and the loading spinner's loader id.
const cmuxKey = "chatchain"

func detectCmux(env Env) Host {
	if env.Getenv("CMUX_SURFACE_ID") == "" {
		return nil
	}
	path, err := env.LookPath("cmux")
	if err != nil {
		return nil
	}
	c := newCmux(func(argv []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, argv...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		_ = cmd.Run() // sidebar dressing must never fail the chat
	})
	sid := env.Getenv("CMUX_SURFACE_ID")
	c.background = func() (bool, bool) { return cmuxBackground(path, sid) }
	return c
}

// DarkBackground implements BackgroundReporter.
func (c *Cmux) DarkBackground() (bool, bool) {
	if c.background == nil {
		return false, false
	}
	return c.background()
}

func newCmux(run func(argv []string)) *Cmux {
	c := &Cmux{exec: run, mail: make(chan [][]string, 1), done: make(chan struct{})}
	go c.loop()
	return c
}

func (c *Cmux) Name() string { return "cmux" }

func (c *Cmux) loop() {
	defer close(c.done)
	for batch := range c.mail {
		for _, argv := range batch {
			c.exec(argv)
		}
	}
}

// post hands the worker a batch, replacing one still waiting — states are
// last-wins, and a stale "Running" must never land after "Idle".
func (c *Cmux) post(batch [][]string) {
	for {
		select {
		case c.mail <- batch:
			return
		default:
			select {
			case <-c.mail: // drop the superseded batch
			default:
			}
		}
	}
}

func (c *Cmux) SetState(s State) { c.post(cmuxBatch(s)) }

// Close clears the status row and spinner — they outlive the process
// otherwise — and waits (bounded) for the worker to flush.
func (c *Cmux) Close() error {
	c.post([][]string{
		{"workspace", "loading", "off", "--id", cmuxKey},
		{"clear-status", cmuxKey},
	})
	close(c.mail)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// cmuxBatch is the CLI sequence for a state. Icons and colors mirror what
// cmux's own Claude Code hook handler sends (bolt/blue running, bell for
// attention, pause/gray idle), pinned by its e2e tests — so the sidebar
// reads the same for chatchain as for the built-in agents.
func cmuxBatch(s State) [][]string {
	switch s {
	case StateBusy:
		return [][]string{
			{"set-status", cmuxKey, "Running", "--icon", "bolt.fill", "--color", "#4C8DFF"},
			{"workspace", "loading", "on", "--id", cmuxKey},
		}
	case StateNeedsInput:
		return [][]string{
			{"workspace", "loading", "off", "--id", cmuxKey},
			{"set-status", cmuxKey, "Needs input", "--icon", "bell.fill"},
		}
	case StateError:
		return [][]string{
			{"workspace", "loading", "off", "--id", cmuxKey},
			{"set-status", cmuxKey, "Failed", "--icon", "exclamationmark.triangle.fill", "--color", "#FF3B30"},
		}
	default: // StateIdle
		return [][]string{
			{"workspace", "loading", "off", "--id", cmuxKey},
			{"set-status", cmuxKey, "Idle", "--icon", "pause.circle.fill", "--color", "#8E8E93"},
		}
	}
}
