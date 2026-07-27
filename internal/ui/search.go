package ui

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
)

// Panel search comes in two flavours, chosen by what the panel IS rather than
// by a caller flag:
//
//   - Row panels (List/Multi/Picker/Browser) FILTER: the query narrows the
//     visible rows and the panel's own keys keep working on what remains, with
//     "c" to clear. Opt-in per panel (Panel.Search) and only once the rows
//     overflow their window — a surface whose options all fit needs no search.
//   - View panels JUMP: the query highlights every hit in place and n/p walk
//     them, the reader's idiom. Always available (again, once the text
//     overflows), since a read-only body has no keys to collide with.
//
// Both share this file's front end: the "/" entry, the mini input that takes
// over the hint row (no new row — the frame's height must not move), and the
// matcher.
type searchMode int

const (
	searchOff     searchMode = iota
	searchTyping             // the query line owns the keyboard
	searchApplied            // row panels: filtered; View: n/p navigation
)

// searchHit is one occurrence in a View panel: the LOGICAL line (pre-wrap) and
// the byte offset of the match within that line's plain text.
type searchHit struct {
	line int
	off  int
}

// searchState is one panel's search. It lives per panel, so tabbing away and
// back leaves a filter (or a highlight) exactly as it was.
type searchState struct {
	mode  searchMode
	query string // the applied query ("" = none)
	input textinput.Model
	ready bool // input constructed

	hits   []searchHit // View only
	hitIdx int

	// matched is how many rows the last rebuild actually matched (row panels).
	// It is NOT len(view): a query matching nothing falls back to showing
	// everything, and the hint row must be able to tell those two apart.
	matched int
}

// ensureInput builds the query field on first use, styled like the Custom
// editor: no prompt, no internal SGR, a real terminal cursor for IME.
func (s *searchState) ensureInput() {
	if s.ready {
		return
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetVirtualCursor(false)
	ti.SetStyles(textinput.Styles{})
	s.input = ti
	s.ready = true
}

// --- matching ---------------------------------------------------------------

// asciiFold lowercases A–Z and nothing else. Byte length is preserved exactly,
// which is what lets match offsets index back into the original string;
// strings.ToLower cannot promise that (U+0130 lowercases to two runes). Case
// only distinguishes ASCII anyway — CJK, the other half of what these panels
// hold, has none.
func asciiFold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// matchRanges returns the non-overlapping [start,end) byte ranges where query
// occurs in plain, case-insensitively.
func matchRanges(plain, query string) [][2]int {
	if query == "" || len(query) > len(plain) {
		return nil
	}
	lp, lq := asciiFold(plain), asciiFold(query)
	var out [][2]int
	for from := 0; from+len(lq) <= len(lp); {
		i := strings.Index(lp[from:], lq)
		if i < 0 {
			break
		}
		s := from + i
		out = append(out, [2]int{s, s + len(lq)})
		from = s + len(lq)
	}
	return out
}

// matchesQuery reports whether query occurs in text (case-insensitively).
func matchesQuery(text, query string) bool {
	return query == "" || strings.Contains(asciiFold(text), asciiFold(query))
}

// --- highlighting -----------------------------------------------------------

const (
	searchHitSGR = "\x1b[7m"    // every hit: reverse video
	searchCurSGR = "\x1b[7;33m" // the hit n/p is parked on: reverse + yellow
)

// highlightLine reverse-videos every occurrence of query in line, which may
// already carry its own SGR (a /debug request row, a syntax-coloured body).
//
// Two things make this more than a string replace. Offsets are computed on the
// PLAIN text and walked back through the escapes, so a highlight can never land
// inside an escape sequence and slice it apart. And a highlight is closed with
// a reset followed by a REPLAY of the line's own SGR state (clip.go's sgrCarry
// tracks it) — without the replay, the first hit in a coloured line would strip
// the colour off everything after it. Escapes met inside a hit re-assert the
// highlight for the same reason, in the other direction.
//
// curOff is the plain-text offset of the hit n/p is parked on (-1 for none),
// which gets the brighter treatment.
func highlightLine(line, query string, curOff int) string {
	if query == "" {
		return line
	}
	ranges := matchRanges(stripSGRText(line), query)
	if len(ranges) == 0 {
		return line
	}

	var b strings.Builder
	state := "" // the line's own SGR, as of this point
	open := ""  // the highlight SGR while one is open
	hlEnd := -1 // plain offset where the open highlight ends
	pos := 0    // plain-text byte offset
	ri := 0

	closeHL := func() {
		b.WriteString(sgrReset)
		if state != "" {
			b.WriteString(state)
		}
		open, hlEnd = "", -1
	}

	for i := 0; i < len(line); {
		if n := ansiLen(line, i); n > 0 {
			seq := line[i : i+n]
			b.WriteString(seq)
			state = sgrCarry(state, seq)
			if hlEnd >= 0 {
				b.WriteString(open) // the line's own escape may have reset us
			}
			i += n
			continue
		}
		if hlEnd >= 0 && pos >= hlEnd {
			closeHL()
		}
		for ri < len(ranges) && ranges[ri][0] < pos {
			ri++
		}
		if hlEnd < 0 && ri < len(ranges) && pos == ranges[ri][0] {
			open = searchHitSGR
			if pos == curOff {
				open = searchCurSGR
			}
			b.WriteString(open)
			hlEnd = ranges[ri][1]
			ri++
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		b.WriteString(line[i : i+size])
		pos += size
		i += size
	}
	if hlEnd >= 0 {
		closeHL()
	}
	return b.String()
}

// --- row-panel plumbing -----------------------------------------------------

// rowCount is how many underlying rows the panel holds, filtering aside.
func (st *panelState) rowCount(p Panel) int {
	if p.Kind == PanelBrowser {
		return len(st.entries)
	}
	return len(st.items)
}

// rowText is row i's text for matching, stripped of any styling the caller
// baked in (the /debug rows arrive pre-coloured).
func (st *panelState) rowText(p Panel, i int) string {
	if p.Kind == PanelBrowser {
		if i < len(st.entries) {
			return st.entries[i].name
		}
		return ""
	}
	if i < len(st.items) {
		return stripSGRText(st.items[i])
	}
	return ""
}

// rebuildView recomputes the visible-row mapping. st.view is ALWAYS populated
// (identity when nothing is filtered), so rendering and navigation have one
// code path rather than two that can drift apart.
//
// Two rows never get filtered out: a Custom panel's "Other…" — it is the way
// to answer when no option fits, so hiding it would strand the user — and
// every row, when the query matches nothing at all. A filter that empties the
// list would leave a cursor pointing at nothing that Enter could still commit.
func (st *panelState) rebuildView(p Panel) {
	n := st.rowCount(p)
	if cap(st.view) < n {
		st.view = make([]int, 0, n)
	}
	st.view = st.view[:0]

	q := ""
	if p.Kind != PanelView && st.search.mode != searchOff {
		q = st.searchDraft()
	}
	if q == "" {
		st.search.matched = n
		for i := 0; i < n; i++ {
			st.view = append(st.view, i)
		}
		return
	}

	otherIdx := -1
	if p.Custom {
		otherIdx = len(p.Items)
	}
	hits := 0
	for i := 0; i < n; i++ {
		if i == otherIdx {
			st.view = append(st.view, i)
			continue
		}
		if matchesQuery(st.rowText(p, i), q) {
			st.view = append(st.view, i)
			hits++
		}
	}
	st.search.matched = hits
	if hits == 0 { // nothing matched: show everything rather than nothing
		st.view = st.view[:0]
		for i := 0; i < n; i++ {
			st.view = append(st.view, i)
		}
	}
}

// filteredCount reports how many rows a live filter keeps; ok is false when
// the query matches nothing (the view then shows everything, unfiltered).
func (st *panelState) filteredCount(p Panel) (int, bool) {
	if st.searchDraft() == "" {
		return st.rowCount(p), true
	}
	return st.search.matched, st.search.matched > 0
}

// searchDraft is the query being typed, or the applied one once committed.
func (st *panelState) searchDraft() string {
	if st.search.mode == searchTyping {
		return st.search.input.Value()
	}
	return st.search.query
}

// viewPos is where the cursor sits among the VISIBLE rows (0 when the cursor's
// row was filtered out from under it).
func (st *panelState) viewPos() int {
	for vp, idx := range st.view {
		if idx == st.cursor {
			return vp
		}
	}
	return 0
}

// setViewPos moves the cursor to visible row vp, translating back to the
// underlying index the caller will read at commit.
func (st *panelState) setViewPos(vp int) {
	if len(st.view) == 0 {
		st.cursor = 0
		return
	}
	st.cursor = st.view[clampInt(vp, 0, len(st.view)-1)]
}

// syncCursor pulls the cursor back onto a visible row after a refilter.
func (st *panelState) syncCursor() {
	if len(st.view) == 0 {
		st.cursor = 0
		return
	}
	for _, idx := range st.view {
		if idx == st.cursor {
			return
		}
	}
	st.cursor = st.view[0]
}

// --- lifecycle --------------------------------------------------------------

// searchAvailable gates "/": row panels need the opt-in flag, View needs
// nothing, and both need content that actually overflows its window — with
// everything on screen there is nothing to search FOR.
func (st *panelState) searchAvailable(p Panel) bool {
	switch p.Kind {
	case PanelView:
		n := len(st.items)
		if p.Wrap && st.wrapped > 0 {
			n = st.wrapped
		}
		return n > panelHeight(p, n)
	case PanelList, PanelMulti, PanelPicker, PanelBrowser:
		if !p.Search {
			return false
		}
		n := st.rowCount(p) // the UNFILTERED count: refining a filter stays possible
		return n > panelHeight(p, n)
	}
	return false
}

// searchOpen starts a FRESH query. The field comes up empty even when a query
// is already applied: "/" means "search for something", and seeding it would
// make the first keystroke append to the old query instead of replacing it.
// Refining is what searchEdit is for.
func (st *panelState) searchOpen(p Panel) {
	st.search.ensureInput()
	st.search.mode = searchTyping
	st.search.input.SetValue("")
	st.search.input.Focus()
	st.searchLive(p)
}

// searchLive re-applies the in-progress query on every keystroke: a row panel
// narrows as you type, a View re-collects its hits and rides to the first one.
// Feedback while typing is the point — a query that matches nothing shows it
// immediately, instead of after an Enter.
func (st *panelState) searchLive(p Panel) {
	if p.Kind == PanelView {
		st.collectHits(p, st.search.input.Value())
		if len(st.search.hits) > 0 {
			st.search.hitIdx = 0
			st.pendingCenter = st.search.hits[0].line
		}
		return
	}
	st.rebuildView(p)
	st.syncCursor()
}

// searchApply commits the query: Enter hands the keyboard back to the panel
// (filtered, for a row panel) or to n/p (for a View).
func (st *panelState) searchApply(p Panel) {
	st.search.query = strings.TrimSpace(st.search.input.Value())
	st.search.input.Blur()
	if st.search.query == "" {
		st.searchClear(p)
		return
	}
	st.search.mode = searchApplied
	if p.Kind == PanelView {
		st.collectHits(p, st.search.query)
		if len(st.search.hits) > 0 {
			st.pendingCenter = st.search.hits[st.search.hitIdx].line
		}
		return
	}
	st.rebuildView(p)
	st.syncCursor()
}

// searchEdit reopens the query field from the applied state (View's q/Esc) to
// REFINE it, so here the applied query is seeded with the cursor after it.
func (st *panelState) searchEdit(p Panel) {
	st.search.ensureInput()
	st.search.mode = searchTyping
	st.search.input.SetValue(st.search.query)
	st.search.input.CursorEnd()
	st.search.input.Focus()
}

// searchClear drops the search entirely: the filter lifts, the highlight goes,
// and the panel is exactly as it was before "/".
func (st *panelState) searchClear(p Panel) {
	st.search.mode = searchOff
	st.search.query = ""
	st.search.hits = nil
	st.search.hitIdx = 0
	if st.search.ready {
		st.search.input.SetValue("")
		st.search.input.Blur()
	}
	if p.Kind != PanelView {
		st.rebuildView(p)
		st.syncCursor()
	}
}

// collectHits finds every occurrence in a View's LOGICAL lines. Wrapping is a
// render-time concern (the width isn't known here), so hits are recorded
// against logical lines and converted when the panel draws.
func (st *panelState) collectHits(p Panel, query string) {
	st.search.hits = nil
	st.search.hitIdx = 0
	if strings.TrimSpace(query) == "" {
		return
	}
	for i, line := range st.items {
		for _, r := range matchRanges(stripSGRText(line), query) {
			st.search.hits = append(st.search.hits, searchHit{line: i, off: r[0]})
		}
	}
}

// recollectHits rescans after a live panel's content changed, keeping the
// walker parked where it was. A plain collectHits would reset it to the first
// hit, and on a panel that refreshes twice a second (/tools, /debug) that
// yanks the highlight back under the reader every tick — n could never reach
// the third hit. The anchor is the hit itself, not its ordinal: content that
// grew above it shifts every index but not the match the user is reading.
func (st *panelState) recollectHits(p Panel, query string) {
	prev := st.currentHit()
	if prev == nil {
		st.collectHits(p, query)
		return
	}
	anchor := *prev
	st.collectHits(p, query)
	for i, h := range st.search.hits {
		if h == anchor {
			st.search.hitIdx = i
			return
		}
	}
	// The anchored hit is gone (its line changed): settle on the first one
	// still at or after where it used to be, so the walker does not leap.
	for i, h := range st.search.hits {
		if h.line > anchor.line || (h.line == anchor.line && h.off > anchor.off) {
			st.search.hitIdx = i
			return
		}
	}
	if n := len(st.search.hits); n > 0 {
		st.search.hitIdx = n - 1
	}
}

// searchStep walks the hits, wrapping around, and asks the next render to
// centre the landing line.
func (st *panelState) searchStep(dir int) {
	n := len(st.search.hits)
	if n == 0 {
		return
	}
	st.search.hitIdx = ((st.search.hitIdx+dir)%n + n) % n
	st.pendingCenter = st.search.hits[st.search.hitIdx].line
}

// currentHit is the hit n/p is parked on, or nil.
func (st *panelState) currentHit() *searchHit {
	if st.search.mode == searchOff || st.search.hitIdx >= len(st.search.hits) {
		return nil
	}
	return &st.search.hits[st.search.hitIdx]
}
