package chat

import (
	"io"
	"os"
	"strings"
	"sync"

	"chatchain/internal/readline"
	"chatchain/provider"

	"golang.org/x/term"
)

// The input composer decorates readline's prompt with two app-rendered lines
// above the "❯ " marker — a full-width separator rule and a live status line
// (model + context-window usage that ticks up as the message is typed). They are
// baked into a fixed-height multi-line prompt so the marker line, which fixes
// readline's cursor column (ppos), never moves; only the status text changes,
// and it is truncated to the terminal width so it can never wrap and add a row.
// This keeps the redraw geometry stable — the failure mode that sank the earlier
// type-ahead attempt (see brain: streaming-typeahead-input) came from a moving
// ppos under concurrent streaming, which does not occur here: nothing else
// writes to the terminal while the user is composing.

// composerChromeRows is how many rows the composer adds above the "❯ " marker
// (the separator + the status line). rewriteUserMessage moves up this many extra
// rows so a sent message's block erases the whole composer, leaving no residue.
const composerChromeRows = 2

// composerTermWidth returns the current terminal width, defaulting to 80.
func composerTermWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// composerPrompt builds the multi-line readline prompt: a separator rule, the
// status line (including the `draft` tokens being composed), and the "❯ " marker
// on its own final line. Only app text lives in the two chrome lines (no user
// CJK), so the wide-rune redraw risk stays confined to the input line, exactly
// as with a plain prompt.
func composerPrompt(p provider.Provider, budget *contextBudget, draft, width int) string {
	sep := DimStyle.Sprint(strings.Repeat("─", width))
	status := composerStatusLine(p, budget, draft, width)
	return sep + "\n" + status + "\n" + UserStyle.Sprint(userPrompt)
}

// composerStatusLine renders the status fields ("model · ctx used/window (pct)")
// indented to the input column (so the text lines up under where typing begins
// after "❯ ") and with a distinct faint hue per field. It stays a single
// physical row: if the fields would exceed the width it falls back to a plain
// faint, truncated form (rare, narrow terminals only).
func composerStatusLine(p provider.Provider, budget *contextBudget, draft, width int) string {
	model := p.Model()
	if model == "" {
		model = p.Type()
	}
	indent := strings.Repeat(" ", displayWidth(userPrompt)) // align under the input, past "❯ "
	ctx := "ctx " + budget.statusWithDraft(draft)
	sep := " · "

	// Measure on the plain text (ANSI is zero-width); only colorize if it fits.
	plain := indent + model + sep + ctx
	if displayWidth(plain) > width {
		return DimStyle.Sprint(truncateWidth(plain, width))
	}
	return indent + StatusModelStyle.Sprint(model) + StatusSepStyle.Sprint(sep) + StatusCtxStyle.Sprint(ctx)
}

// composerEraser wraps the chat output writer so a live input composer is
// erased exactly once, just before the turn's first real output lands — with no
// per-command code. The rule is purely behavioral:
//
//   - After a submit the loop arms the eraser with the echoed input line.
//   - Interactive TUIs (promptui Viewer/Tabbed) render through their OWN
//     readline to os.Stdout, NOT through this writer, so they never disarm it —
//     the composer stays visible as a frame above them.
//   - The first write that DOES go through here (a command's printed output, a
//     mixed command's post-TUI line, or a message's user block) erases the
//     composer first, then passes the bytes through.
//   - A turn that produces no output at all (a pure viewer, or a blank line)
//     leaves the eraser armed; the loop calls flush at the top of the next turn
//     to erase it before the fresh composer is drawn.
//
// So any new command is handled automatically: print and it clears up front,
// open a TUI and the composer frames it, then clears the moment real output
// appears (or at the next flush).
type composerEraser struct {
	under io.Writer
	mu    sync.Mutex
	raw   string // echoed input line of the armed composer (for echoRows)
	armed bool
}

func newComposerEraser(under io.Writer) *composerEraser { return &composerEraser{under: under} }

func (c *composerEraser) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.armed {
		eraseComposer(c.under, c.raw)
		c.armed = false
	}
	c.mu.Unlock()
	return c.under.Write(p)
}

// arm marks the on-screen composer (echoed input = raw) for erasure before the
// next write or at the next flush.
func (c *composerEraser) arm(raw string) {
	c.mu.Lock()
	c.raw, c.armed = raw, true
	c.mu.Unlock()
}

// flush erases an armed composer now — used at the top of each turn so a
// previous turn that produced no output (a self-cleaning TUI or a blank line)
// does not leave its composer behind under the fresh one.
func (c *composerEraser) flush() {
	c.mu.Lock()
	if c.armed {
		eraseComposer(c.under, c.raw)
		c.armed = false
	}
	c.mu.Unlock()
}

// composerChromeListener wraps base with the live-status behavior. After each
// keystroke it recomputes the draft token count and, only when the rendered
// prompt actually changes, updates the prompt and forces a full repaint so the
// new status shows; unchanged keystrokes fall through to base and readline's
// cheap end-of-line append path. getRL fetches the readline instance lazily
// (it is assigned after NewEx, once, so the closure captures it by reference).
func composerChromeListener(getRL func() *readline.Instance, p provider.Provider, budget *contextBudget, base readline.Listener) readline.Listener {
	var last string
	return func(line []rune, pos int, key rune) ([]rune, int, bool) {
		width := composerTermWidth()
		draft := budget.counter.count(string(line))
		prompt := composerPrompt(p, budget, draft, width)
		changed := prompt != last
		if changed {
			if rl := getRL(); rl != nil {
				rl.SetPrompt(prompt)
			}
			last = prompt
		}
		// Let base run first: it keeps lineEmpty in sync, reserves bottom headroom,
		// and repaints command lines. If base rewrites the line, that takes priority.
		if base != nil {
			if nl, np, ok := base(line, pos, key); ok {
				return nl, np, ok
			}
		}
		// Force a repaint for a real keystroke whose status changed. The init call
		// (key==0, line==nil) only seeds the prompt: it runs before the first
		// Print, so no repaint is needed and returning ok=true there would clobber
		// the (empty) buffer.
		if changed && key != 0 {
			return line, pos, true
		}
		return nil, 0, false
	}
}
