package ui

import (
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
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
// above, the last rows REPLACE the preview in place. When the replacement is
// SHORTER than the preview (a reasoning window collapsing to its one-line
// "thought for Ns" marker), the uncovered preview rows stay put as dim
// residue and later lines consume them top-down — never entering scrollback.
// The window's height only ever grows to tailKeep and then stays constant, so
// the composer never pops upward — the bounce class is gone by construction.
//
// The window never closes: at idle it shows the last lines of the previous
// reply (visually indistinguishable from scrollback); Close flushes it. The
// only remaining shrinks are an interrupted preview and end-of-turn residue
// (both through dropPreview), one-frame artifacts on turn boundaries.
//
// All mutations emit Println (overflow) + a regionMsg snapshot to the model
// under one mutex, so concurrent writers (stream goroutine, MCP reporter)
// keep global ordering.
type region struct {
	mu      sync.Mutex
	u       *UI
	emit    func(over []string, snap regionMsg) // test seam; nil = via u.p
	tail    []string                            // committed-pending lines shown in the frame
	residue []string                            // stale preview rows awaiting in-place replacement
	label   string                              // preview header label ("" = no preview)
	ptail   []string                            // preview rolling source lines (≤ previewWindow)
	open    bool                                // preview receiving lines (false once closed/deferred)
	since   time.Time                           // call preview: lifecycle start (zero = plain preview)
	detail  string                              // call preview: live status-row prefix ("1.2k tokens")
}

// regionMsg is the display snapshot the model renders.
type regionMsg struct {
	tail    []string
	residue []string
	label   string
	ptail   []string
	since   time.Time // non-zero: render the "⎿ [detail ·] Ns · ESC" row and tick
	detail  string
}

func (r *region) previewRowsLocked() int {
	if r.label == "" {
		return 0
	}
	n := 1 + len(r.ptail)
	if !r.since.IsZero() {
		n++ // the model-rendered "⎿ elapsed" status row
	}
	return n
}

// consumeResidueLocked lets n freshly displayed rows overwrite the oldest
// residue rows — the in-place replacement that keeps the window height flat.
func (r *region) consumeResidueLocked(n int) {
	if n >= len(r.residue) {
		r.residue = nil
		return
	}
	r.residue = r.residue[n:]
}

// rebalanceLocked keeps len(tail)+residue+previewRows ≤ tailKeep by
// overflowing the oldest tail lines; returns the overflow to Println.
func (r *region) rebalanceLocked() []string {
	var over []string
	for len(r.tail)+len(r.residue)+r.previewRowsLocked() > tailKeep && len(r.tail) > 0 {
		over = append(over, r.tail[0])
		r.tail = r.tail[1:]
	}
	return over
}

func (r *region) snapshotLocked() regionMsg {
	return regionMsg{
		tail:    append([]string{}, r.tail...),
		residue: append([]string{}, r.residue...),
		label:   r.label,
		ptail:   append([]string{}, r.ptail...),
		since:   r.since,
		detail:  r.detail,
	}
}

// publishLocked emits the overflow and the fresh snapshot. Println+Send stay
// under the caller's lock so interleaved writers cannot reorder history.
//
// The overflow is CHUNKED below the screen height: the renderer's insertAbove
// scrolls the screen by the block's row count and compensates with
// InsertLine(n), which the terminal clamps to the screen height — a single
// insert taller than the screen permanently desyncs the frame anchor (the
// composer separators and status line get eaten). Seen in the wild with a big
// /session resume echo. Half the screen per Println keeps headroom for the
// occasional line that still wraps.
func (r *region) publishLocked(over []string) {
	if len(over) > 0 {
		debugRegion("  overflow %q", over)
	}
	over = r.sanitizeOverflow(over)
	if r.emit != nil {
		r.emit(over, r.snapshotLocked())
		return
	}
	for _, chunk := range chunkOverflow(over, r.screenHeight()) {
		r.u.p.Println(joinOverflow(chunk))
	}
	r.u.p.Send(r.snapshotLocked())
}

// sanitizeOverflow trims one column off any line whose display width is an
// exact multiple of the screen width. bubbletea's insertAbove counts such a
// line as one row taller than it renders (offset += lineWidth/w, but the
// terminal's deferred wrap fits k·w columns on k rows, not k+1) — every such
// line entering scrollback over-scrolls the screen by one and leaves a ghost
// copy of a frame row behind (user blocks, markdown rules, and table borders
// are all exactly full-width). Seen as duplicated turns after a /session
// resume echo.
func (r *region) sanitizeOverflow(over []string) []string {
	w := r.screenWidth()
	if w <= 1 {
		return over
	}
	for i, ln := range over {
		if lw := ansi.StringWidth(ln); lw > 0 && lw%w == 0 {
			over[i] = ansi.Truncate(ln, lw-1, "")
		}
	}
	return over
}

func (r *region) screenHeight() int {
	if r.u == nil {
		return 24
	}
	if h := int(r.u.height.Load()); h > 0 {
		return h
	}
	return 24
}

func (r *region) screenWidth() int {
	if r.u == nil {
		return 0
	}
	return int(r.u.width.Load())
}

// splitRows expands entries with embedded newlines into one entry per row.
// All window bookkeeping (tail height, rebalance, overflow row counts — and
// through them the frame anchor and the composer cursor) assumes one visual
// row per entry; a multi-line entry silently desyncs them all.
func splitRows(lines []string) []string {
	clean := true
	for _, ln := range lines {
		if strings.ContainsRune(ln, '\n') {
			clean = false
			break
		}
	}
	if clean {
		return lines
	}
	out := make([]string, 0, len(lines)+4)
	for _, ln := range lines {
		out = append(out, strings.Split(ln, "\n")...)
	}
	return out
}

// oneRow collapses embedded line breaks into spaces. Every frame row the
// model renders — the preview label, its rolling source lines, the status-row
// detail — is counted as exactly one visual line in rowsAbove (the composer
// cursor offset); a newline smuggled in by a producer would desync the frame
// anchor just like an unsplit multi-line commit. splitRows guards the tail;
// this guards the preview-side entries, which are single-row by definition.
func oneRow(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\r", " ")
}

// chunkOverflow splits lines into batches of at most max(2, h/2) lines.
func chunkOverflow(over []string, h int) [][]string {
	if len(over) == 0 {
		return nil
	}
	size := h / 2
	if size < 2 {
		size = 2
	}
	var chunks [][]string
	for start := 0; start < len(over); start += size {
		end := start + size
		if end > len(over) {
			end = len(over)
		}
		chunks = append(chunks, over[start:end])
	}
	return chunks
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
// preview present, this is the in-place morph: the lines cover the header row
// first, then the preview rows top-down; preview rows the block's lines don't
// reach stay as residue for later lines — height constant either way. A call
// preview's status row contributes a blank placeholder so its vanishing never
// shrinks the window.
func (r *region) commit(lines []string) {
	if len(lines) == 0 {
		return
	}
	lines = splitRows(lines)
	r.mu.Lock()
	defer r.mu.Unlock()
	debugRegion("commit %q label=%q open=%v residue=%d", lines, r.label, r.open, len(r.residue))
	if r.label != "" && !r.open {
		// Deferred preview close: its replacement content has arrived.
		rows := r.ptail
		if !r.since.IsZero() {
			rows = append(append([]string{}, r.ptail...), "")
		}
		if covered := len(lines) - 1; covered < len(rows) {
			r.residue = append(r.residue, rows[covered:]...)
		}
		r.label = ""
		r.ptail = nil
		r.since = time.Time{}
		r.detail = ""
	} else {
		r.consumeResidueLocked(len(lines))
	}
	r.tail = append(r.tail, lines...)
	over := r.rebalanceLocked()
	r.publishLocked(over)
}

// openPreview starts a block preview (header + rolling source lines). Opening
// over an existing preview folds that one into residue — the new header takes
// the old header's row, the old rows await replacement — so back-to-back
// previews never move the composer.
func (r *region) openPreview(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.foldOpenLocked(label)
	r.since = time.Time{}
	over := r.rebalanceLocked() // header row may steal a tail line
	r.publishLocked(over)
}

// openCallPreview ensures the tool-call lifecycle widget: a header plus the
// model-rendered "⎿ [detail ·] elapsed · ESC" status row. When a call preview
// is already open this relabels it in place (the clock and detail keep
// running — a composing call expanding to its full header); otherwise it
// fold-opens fresh with a cleared detail.
func (r *region) openCallPreview(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	debugRegion("openCallPreview %q ensure=%v", label, r.label != "" && !r.since.IsZero())
	if r.label != "" && !r.since.IsZero() {
		r.label = oneRow(label)
		r.open = true
		r.publishLocked(nil)
		return
	}
	r.foldOpenLocked(label)
	r.since = time.Now()
	r.detail = ""
	over := r.rebalanceLocked()
	r.publishLocked(over)
}

// setCallBody replaces the call widget's body rows wholesale (progressive
// image frames: each partial supersedes the last). Bounded by the caller;
// rows go through oneRow like every preview entry. No-op unless a call
// preview is up and receiving.
func (r *region) setCallBody(rows []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.label == "" || r.since.IsZero() || !r.open {
		return
	}
	out := make([]string, len(rows))
	for i, ln := range rows {
		out[i] = oneRow(ln)
	}
	r.ptail = out
	over := r.rebalanceLocked()
	r.publishLocked(over)
}

// setCallDetail updates the call preview's live status-row prefix ("1.2k
// tokens"); a no-op unless a call preview is up and still receiving — a
// throttled meter update racing the settle must not resurrect the row.
func (r *region) setCallDetail(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.label == "" || r.since.IsZero() || !r.open {
		return
	}
	r.detail = oneRow(detail)
	r.publishLocked(nil)
}

// foldOpenLocked replaces any current preview with a fresh one, folding the
// old rows (and a status-row placeholder) into residue.
func (r *region) foldOpenLocked(label string) {
	if r.label != "" {
		r.residue = append(r.residue, r.ptail...)
		if !r.since.IsZero() {
			r.residue = append(r.residue, "")
		}
	} else {
		r.consumeResidueLocked(1) // the header row overwrites a residue row
	}
	r.label = oneRow(label)
	r.ptail = nil
	r.open = true
}

// previewLine appends a raw source line to the rolling preview window.
func (r *region) previewLine(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.open {
		return
	}
	r.ptail = append(r.ptail, oneRow(line))
	if len(r.ptail) > previewWindow {
		r.ptail = r.ptail[len(r.ptail)-previewWindow:]
	} else {
		r.consumeResidueLocked(1) // a genuinely new row overwrites a residue row
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
	debugRegion("closePreview label=%q", r.label)
	r.open = false
}

// dropPreview discards a preview and any residue outright (interrupted turn,
// end of turn): the one shrink this design keeps, on turn boundaries.
func (r *region) dropPreview() {
	r.mu.Lock()
	defer r.mu.Unlock()
	debugRegion("dropPreview label=%q residue=%d", r.label, len(r.residue))
	if r.label == "" && len(r.residue) == 0 {
		return
	}
	r.label = ""
	r.ptail = nil
	r.residue = nil
	r.open = false
	r.since = time.Time{}
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
// full transcript). Preview rows and residue are display-only — dropped, not
// flushed.
func (r *region) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	over := r.tail
	r.tail = nil
	r.residue = nil
	if r.label != "" {
		r.label = ""
		r.ptail = nil
	}
	r.since = time.Time{}
	r.publishLocked(over)
}
