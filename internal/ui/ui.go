// Package ui is the terminal interaction layer: a bubbletea v2 INLINE program
// owning a pinned bottom frame (status + real-cursor composer, with transient
// surfaces below it) while committed history flows above into native
// scrollback via Println. It is the ONLY package in the module allowed to
// import the charm.land event-loop stack (bubbletea/bubbles).
//
// Logic talks to it through a synchronous facade (UI): input-acquiring calls
// (ReadInput/Select/View/Confirm) block on buffered reply channels; everything
// else (PrintLines/UserBlock/StreamSink/SetStatus/...) is fire-and-forget with
// ordering guaranteed by the Program mailbox. The full contract lives in
// docs/design/ui-architecture.md.
package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"chatchain/internal/textwidth"
)

// ErrClosed is returned by blocking calls when the UI has shut down.
var ErrClosed = errors.New("ui: closed")

// ErrInterrupted is returned by ReadInput on an idle Ctrl+C/Ctrl+D — the
// caller's cue to exit the session loop.
var ErrInterrupted = errors.New("ui: interrupted")

// Input is one submitted line. Display is the transcript echo: pasted blocks
// expanded so the user sees what was sent, each trimmed to a readable head.
// Text is what logic processes — every paste expanded in full.
type Input struct {
	Display string
	Text    string
}

// StatusData feeds the live status line under the separator.
type StatusData struct {
	Model     string
	CtxUsed   int
	CtxWindow int
	Estimated bool // token figure is a local estimate (≈ prefix)
}

// ProgressState drives the terminal's NATIVE progress indicator (ConEmu
// OSC 9;4 — Ghostty draws a bar at the top of the window, Windows Terminal a
// tab ring + taskbar state, iTerm2 and kitty their own): Busy while a turn
// works, Input while it is blocked on the user, Error when it failed. The
// bubbletea renderer owns emission — it diffs states and clears the bar on
// exit — so this never touches raw escapes.
type ProgressState int

const (
	ProgressNone  ProgressState = iota
	ProgressBusy                // turn streaming / tools running → indeterminate
	ProgressInput               // blocked on the user (approval gate) → warning
	ProgressError               // turn failed → error, until the user acts
)

// SelectSpec is the minimal single-select surface (P2); tabbed/multi/slider
// fidelity arrives in P3.
type SelectSpec struct {
	Title  string
	Items  []string
	Cursor int // initial cursor index
}

// SelectResult reports the selection; Cancelled on ESC/q/Ctrl+C.
type SelectResult struct {
	Index     int
	Cancelled bool
}

// ViewSpec is a read-only text viewer (↑↓ scroll, q/ESC close).
type ViewSpec struct {
	Title  string
	Lines  []string
	Height int // visible rows; 0 = min(len(Lines), 15)
}

// StreamSink is a turn's scope handle plus the markdown renderer's preview
// seam: BlockPreview opens the live rolling preview in the frame (raw source
// lines; Close clears it; labels render as given — pre-style them), and Done
// ends the turn scope. Committed lines flow through the chat transcript
// (ui.PrintLines); the lifecycle-widget verbs (CallPreview/CallDetail/
// ClosePreview) live on UI itself.
type StreamSink interface {
	BlockPreview(label string) io.WriteCloser
	Done()
}

// CallPreview ensures the lifecycle widget: a spinner header over a live
// "⎿ [detail ·] elapsed · ESC to cancel" row. When a call widget is already
// open this relabels it in place (clock and detail keep running); otherwise
// it fold-opens fresh.
func (u *UI) CallPreview(label string) { u.region.openCallPreview(label) }

// CallDetail updates the widget's live status-row prefix ("1.2k tokens").
func (u *UI) CallDetail(detail string) { u.region.setCallDetail(detail) }

// CallBody replaces the call widget's body rows (progressive image frames).
func (u *UI) CallBody(rows []string) { u.region.setCallBody(rows) }

// CallLine appends one row to the call widget's rolling body (a completed
// activity event scrolling through the group panel).
func (u *UI) CallLine(line string) { u.region.previewLine(line) }

// PauseClock freezes the call widget's elapsed figure while the user is
// being consulted; ResumeClock continues it where it froze.
func (u *UI) PauseClock()  { u.region.pauseClock() }
func (u *UI) ResumeClock() { u.region.resumeClock() }

// Height reports the terminal height in rows (0 before the first resize).
func (u *UI) Height() int { return int(u.height.Load()) }

// ClosePreview deferred-closes whatever preview is open: the next committed
// lines morph it away in place.
func (u *UI) ClosePreview() { u.region.closePreview() }

// UI is the facade handle. Safe for concurrent use.
type UI struct {
	p      *tea.Program
	done   chan struct{}
	err    error // Program.Run result; read after done closes
	width  atomic.Int64
	height atomic.Int64
	region region // the output staging window (see region.go)
}

// New starts the inline Program and returns the facade. Close releases the
// terminal.
func New() *UI {
	u := &UI{done: make(chan struct{})}
	u.region.u = u
	u.width.Store(80)
	u.height.Store(24)
	m := newModel(&u.width, &u.height)
	m.flushTail = u.region.flushTail
	u.p = tea.NewProgram(m)
	go func() {
		_, err := u.p.Run()
		u.err = err
		close(u.done)
	}()
	return u
}

// Close flushes the staging window into scrollback (the transcript must be
// complete), shuts the Program down, and waits for the terminal release.
func (u *UI) Close() error {
	u.region.flush()
	u.p.Quit()
	<-u.done
	return u.err
}

// Done is closed when the Program has exited — background reporters select on
// it so they never write into a dead Program.
func (u *UI) Done() <-chan struct{} { return u.done }

// ReadInput blocks until the user submits a line (or a queued type-ahead
// submit is pending — the queue drains in order). Idle Ctrl+C/Ctrl+D surface
// as ErrInterrupted.
func (u *UI) ReadInput(ctx context.Context) (Input, error) {
	reply := make(chan inputResult, 1)
	u.p.Send(readReqMsg{reply: reply})
	select {
	case r := <-reply:
		return r.in, r.err
	case <-ctx.Done():
		u.p.Send(readCancelMsg{reply: reply})
		return Input{}, ctx.Err()
	case <-u.done:
		return Input{}, ErrClosed
	}
}

// PrintLines commits styled lines to scrollback (above the frame).
func (u *UI) PrintLines(lines ...string) {
	if len(lines) == 0 {
		return
	}
	u.region.commit(lines)
}

// UserBlock commits a sent message as full-width reversed rows with a "❯ "
// gutter — the visual echo of a submitted input.
func (u *UI) UserBlock(display string) {
	w := int(u.width.Load())
	if w < 8 {
		w = 8
	}
	gutter := 2
	rows := wrapByWidth(display, w-gutter)
	styled := make([]string, len(rows))
	for i, row := range rows {
		g := "  "
		if i == 0 {
			g = "❯ "
		}
		pad := w - gutter - textwidth.StringWidth(row)
		if pad < 0 {
			pad = 0
		}
		styled[i] = revOn + g + row + strings.Repeat(" ", pad) + sgrReset
	}
	u.region.commit(styled)
}

// StartStream opens a turn's output sink and registers its cancel on the
// interrupt scope stack (ESC/Ctrl+C route to it). Call Done to close the
// scope.
func (u *UI) StartStream(cancel context.CancelFunc) StreamSink {
	u.p.Send(scopePushMsg{cancel: cancel})
	return &streamSink{u: u}
}

// Busy shows a frame spinner with label (elapsed time appended by ui); the
// returned stop clears it. The status line is the single home for live turn
// state (thinking, sending, composing tool calls, running tools): one fixed
// row, so state changes never move the composer.
func (u *UI) Busy(label string) (stop func()) {
	u.p.Send(busyOnMsg{label: label})
	return func() { u.p.Send(busyOffMsg{}) }
}

// BusyDetail updates the live sub-state of the current busy phase ("1.2/5.0
// MB", "4.2 KB") without resetting its clock. No-op when nothing is busy.
func (u *UI) BusyDetail(detail string) {
	u.p.Send(busyDetailMsg(detail))
}

// PushCancelScope registers an inner interrupt scope (e.g. one tool call):
// ESC fires the innermost scope, Ctrl+C the turn. The returned pop removes it
// (idempotent pairing is the caller's job — pop exactly once).
func (u *UI) PushCancelScope(cancel context.CancelFunc) (pop func()) {
	u.p.Send(scopePushMsg{cancel: cancel})
	return func() { u.p.Send(scopePopMsg{}) }
}

// Width reports the last known terminal width (80 before the first resize).
func (u *UI) Width() int { return int(u.width.Load()) }

// SetStatus updates the live status line fields.
func (u *UI) SetStatus(s StatusData) { u.p.Send(statusMsg(s)) }

// SetTitle sets the terminal window title.
func (u *UI) SetTitle(title string) { u.p.Send(titleMsg(sanitizeWindowTitle(title))) }

// SetProgress updates the terminal's native progress indicator.
func (u *UI) SetProgress(s ProgressState) { u.p.Send(progressMsg(s)) }

// Notify pings the user — only while the terminal is UNFOCUSED (focus
// reporting): whoever is already watching needs no bell. Both standard
// channels fire in one write, an OSC 9 desktop notification carrying the
// text and a BEL; the terminal's own settings decide presentation. The
// global on/off policy lives ABOVE this facade (internal/host.Presenter) —
// here only the focus mechanism gates.
func (u *UI) Notify(text string) { u.p.Send(notifyMsg(sanitizeWindowTitle(text))) }

// sanitizeWindowTitle strips control characters and bounds the length for a
// tab label. The renderer emits the title as a raw OSC sequence
// (ansi.SetWindowTitle does no escaping), so a crafted session title carrying
// ESC/BEL bytes could otherwise terminate or escape the sequence.
func sanitizeWindowTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return s
}

// SetSlashCommands installs the slash-command table backing the composer's
// suggestion row and Tab completion.
func (u *UI) SetSlashCommands(cmds []string) { u.p.Send(setCommandsMsg(append([]string{}, cmds...))) }

// Tabbed opens a multi-tab surface below the composer (Tab switches panels,
// Enter commits ALL tabs, ESC/q cancels) and blocks until it resolves.
//
// The staging tail flushes to scrollback first: opening a surface grows the
// frame by the panel height, and — exactly like a resize reflow — the rows
// the frame previously occupied can be left behind as ghost copies. Flushing
// shrinks the ghostable surface to the separator (the resize mitigation,
// region.flushTail); the mailbox is FIFO, so the flush lands before the open.
func (u *UI) Tabbed(ctx context.Context, spec TabbedSpec) (TabbedResult, error) {
	u.region.flushTail()
	reply := make(chan TabbedResult, 1)
	u.p.Send(tabbedOpenMsg{spec: spec, reply: reply})
	select {
	case r := <-reply:
		return r, nil
	case <-ctx.Done():
		u.p.Send(surfaceCancelMsg{})
		return TabbedResult{Cancelled: true}, ctx.Err()
	case <-u.done:
		return TabbedResult{Cancelled: true}, ErrClosed
	}
}

// Select opens the single-select surface below the composer and blocks until
// a choice or cancel (a one-panel Tabbed).
//
// Search is on unconditionally here, unlike a hand-built Panel: this facade
// exists to pick one row out of a list, so a list long enough to scroll is
// always worth searching. Nothing is spent on the short ones — searchAvailable
// keeps "/" unbound until the rows overflow their window.
func (u *UI) Select(ctx context.Context, spec SelectSpec) (SelectResult, error) {
	r, err := u.Tabbed(ctx, TabbedSpec{Panels: []Panel{{
		Title: spec.Title, Kind: PanelList, Items: spec.Items, Cursor: spec.Cursor,
		Search: true,
	}}})
	if err != nil || r.Cancelled {
		return SelectResult{Cancelled: true}, err
	}
	return SelectResult{Index: r.Panels[0].Cursor}, nil
}

// View opens the read-only viewer below the composer and blocks until closed
// (a one-panel Tabbed).
func (u *UI) View(ctx context.Context, spec ViewSpec) error {
	_, err := u.Tabbed(ctx, TabbedSpec{Panels: []Panel{{
		Title: spec.Title, Kind: PanelView, Lines: spec.Lines, Height: spec.Height,
	}}})
	return err
}

// RunSurface runs a one-shot, surface-only Program: it renders just the
// tabbed surface (no composer chrome), blocks until the user commits or
// cancels, and releases the terminal. Pre-REPL interactions (--resume
// session picker) use it; in-REPL surfaces go through UI.Tabbed.
func RunSurface(spec TabbedSpec) (TabbedResult, error) {
	m := newModel(nil, nil)
	m.oneShot = true
	reply := make(chan TabbedResult, 1)
	m.surf = newSurface(spec, reply, 1)
	_, err := tea.NewProgram(m).Run()
	select {
	case r := <-reply:
		return r, err
	default:
		return TabbedResult{Cancelled: true}, err
	}
}

// Confirm is a two-item Select returning true for the first (yes) item.
func (u *UI) Confirm(ctx context.Context, title, yes, no string) (bool, error) {
	r, err := u.Select(ctx, SelectSpec{Title: title, Items: []string{yes, no}})
	if err != nil {
		return false, err
	}
	return !r.Cancelled && r.Index == 0, nil
}

// wrapByWidth hard-wraps plain text to rows of at most width columns,
// preserving explicit newlines (CJK-aware via textwidth).
func wrapByWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		var row strings.Builder
		col := 0
		for _, r := range line {
			rw := textwidth.RuneWidth(r)
			if col+rw > width && col > 0 {
				rows = append(rows, row.String())
				row.Reset()
				col = 0
			}
			row.WriteRune(r)
			col += rw
		}
		rows = append(rows, row.String())
	}
	return rows
}
