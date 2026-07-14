package ui

import (
	"strings"
	"sync"
)

// tailKeep is the staging window height: the last N output lines live INSIDE
// the frame (directly above the separator) instead of scrollback.
const tailKeep = 4

// region is the output staging window — the answer to "why can't the preview
// area just be overwritten in place?". All committed output flows THROUGH it:
// new lines enter the window, older lines overflow into real scrollback
// (Println). A block preview occupies window rows by stealing them (each
// steal commits one tail line — a growth, never a shrink), and when the block
// flushes, its rendered lines flow through the same window: the head commits
// above, the last rows REPLACE the preview in place. The window's height only
// ever grows to tailKeep and then stays constant, so the composer never pops
// upward — the bounce class is gone by construction.
//
// The window never closes: at idle it shows the last lines of the previous
// reply (visually indistinguishable from scrollback); Close flushes it. The
// only remaining shrink is an interrupted preview (dropPreview), a one-frame
// artifact on an already-disruptive action.
//
// All mutations emit Println (overflow) + a regionMsg snapshot to the model
// under one mutex, so concurrent writers (stream goroutine, MCP reporter)
// keep global ordering.
type region struct {
	mu    sync.Mutex
	u     *UI
	emit  func(over []string, snap regionMsg) // test seam; nil = via u.p
	tail  []string                            // committed-pending lines shown in the frame
	label string                              // preview header label ("" = no preview)
	ptail []string                            // preview rolling source lines (≤ previewWindow)
	open  bool                                // preview receiving lines (false once closed/deferred)
}

// regionMsg is the display snapshot the model renders.
type regionMsg struct {
	tail  []string
	label string
	ptail []string
}

func (r *region) previewRowsLocked() int {
	if r.label == "" {
		return 0
	}
	return 1 + len(r.ptail)
}

// rebalanceLocked keeps len(tail)+previewRows ≤ tailKeep by overflowing the
// oldest tail lines; returns the overflow to Println.
func (r *region) rebalanceLocked() []string {
	var over []string
	for len(r.tail)+r.previewRowsLocked() > tailKeep && len(r.tail) > 0 {
		over = append(over, r.tail[0])
		r.tail = r.tail[1:]
	}
	return over
}

func (r *region) snapshotLocked() regionMsg {
	return regionMsg{
		tail:  append([]string{}, r.tail...),
		label: r.label,
		ptail: append([]string{}, r.ptail...),
	}
}

// publishLocked emits the overflow and the fresh snapshot. Println+Send stay
// under the caller's lock so interleaved writers cannot reorder history.
func (r *region) publishLocked(over []string) {
	if r.emit != nil {
		r.emit(over, r.snapshotLocked())
		return
	}
	if len(over) > 0 {
		r.u.p.Println(joinOverflow(over))
	}
	r.u.p.Send(r.snapshotLocked())
}

// joinOverflow joins overflow lines for one Println. bubbletea's insertAbove
// drops empty strings, but a lone blank separator (markdown's block spacing)
// must still land in scrollback — a single space renders identically.
func joinOverflow(over []string) string {
	s := strings.Join(over, "\n")
	if s == "" {
		return " "
	}
	return s
}

// commit flows lines through the window. With a (possibly deferred-closed)
// preview present, this is the in-place morph: the preview rows are replaced
// by the block's last lines, earlier lines overflow above — height constant.
func (r *region) commit(lines []string) {
	if len(lines) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.label != "" && !r.open {
		// Deferred preview close: its replacement content has arrived.
		r.label = ""
		r.ptail = nil
	}
	r.tail = append(r.tail, lines...)
	over := r.rebalanceLocked()
	r.publishLocked(over)
}

// openPreview starts a block preview (header + rolling source lines).
func (r *region) openPreview(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.label = label
	r.ptail = nil
	r.open = true
	over := r.rebalanceLocked() // header row may steal a tail line
	r.publishLocked(over)
}

// previewLine appends a raw source line to the rolling preview window.
func (r *region) previewLine(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.open {
		return
	}
	r.ptail = append(r.ptail, line)
	if len(r.ptail) > previewWindow {
		r.ptail = r.ptail[len(r.ptail)-previewWindow:]
	}
	over := r.rebalanceLocked() // growth steals tail lines: commit, never shrink
	r.publishLocked(over)
}

// closePreview marks the preview finished but KEEPS it on screen: the next
// commit replaces it in place (the morph). This is what makes the flush
// bounce-free — nothing shrinks between the source window and the rendered
// block.
func (r *region) closePreview() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open = false
}

// dropPreview discards a preview outright (interrupted turn): the one shrink
// this design keeps, on an already-disruptive path.
func (r *region) dropPreview() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.label == "" {
		return
	}
	r.label = ""
	r.ptail = nil
	r.open = false
	r.publishLocked(nil)
}

// flushTail commits the staged tail into scrollback while keeping an open
// preview. Called on terminal resize: reflow ghosts duplicate the frame's TOP
// rows once per resize event, and with the staging window those rows are
// conversation content — flushing first shrinks the ghost surface to the
// separator. The tail refills from subsequent output.
func (r *region) flushTail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tail) == 0 {
		return
	}
	over := r.tail
	r.tail = nil
	r.publishLocked(over)
}

// flush commits everything still staged (shutdown: scrollback must hold the
// full transcript).
func (r *region) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	over := r.tail
	r.tail = nil
	if r.label != "" {
		r.label = ""
		r.ptail = nil
	}
	r.publishLocked(over)
}
