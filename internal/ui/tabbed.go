package ui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textinput"

	"charm.land/lipgloss/v2"
	"chatchain/internal/textwidth"
	"github.com/charmbracelet/x/ansi"
)

// PanelKind selects a tabbed panel's behavior — the v2 ports of the vendored
// promptui panels (list, checkbox multi-select, slider, directory browser,
// read-only view) plus the v2-native one-line input and boolean switch.
type PanelKind int

const (
	PanelList PanelKind = iota
	PanelMulti
	PanelSlider
	PanelSwitch
	PanelInput
	PanelBrowser
	PanelPicker
	PanelView
)

// Panel describes one tab of a Tabbed surface.
type Panel struct {
	Title string
	Kind  PanelKind

	// Prompt is an optional one-line body text rendered dim between the tab
	// bar and the panel content — the ask toolset's question text (the Title
	// stays a short tab chip).
	Prompt string

	// List / Multi.
	Items   []string
	Cursor  int   // initial cursor (List)
	Checked []int // initially checked indices (Multi)
	// Custom appends an inline "Other…" free-text choice: Enter (or Space on
	// a Multi) on that row opens an inline input IN PLACE; Enter confirms the
	// text (single-select proceeds with it, Multi checks the row and stays),
	// ESC closes just the editor. The typed text returns in Result.Custom.
	Custom bool

	// Slider (←/→ step, g default, G max).
	Min, Max, Step float64
	Value          *float64 // nil = default

	// Switch (Space toggles, ←/→ set off/on). On is the initial state.
	On bool

	// View.
	Lines []string
	Wrap  bool

	// Search offers "/" on a row panel (List/Multi/Picker/Browser), where it
	// FILTERS the rows to those matching the query — see search.go. Off by
	// default, and inert until the rows overflow their window: a short list
	// needs no search, and the key is better left unbound there. View panels
	// search unconditionally (they jump-and-highlight instead, colliding with
	// nothing) and ignore this field.
	Search bool

	// Height caps the panel's visible rows (0 = default).
	Height int

	// Browser starting directory ("" = working directory).
	Dir string

	// Input: a one-line text field with a subtle background; content beyond
	// InputWidth scrolls horizontally. Text is the initial value.
	Text        string
	Placeholder string
	InputWidth  int // visible columns (0 = 40), clamped to the terminal

	// Picker: a single-select list (Items) with a preview pane beside it.
	// Preview renders the highlighted item into at most maxCols×maxRows cells
	// (self-contained SGR rows, e.g. imgterm half-blocks); it is called only
	// when the selection or the pane geometry changes, so an expensive
	// renderer runs once per selection, not once per frame. A nil Preview —
	// or a terminal too narrow for two columns — degrades to a plain list.
	Preview func(index, maxCols, maxRows int) []string

	// Details supplies one dim line per item, rendered under the panel body
	// and following the cursor (the picker's file path). Entries may carry
	// their own escapes (OSC 8 links) and are printed verbatim, so callers
	// must size the visible text themselves — truncating a hyperlink here
	// would slice the escape sequence apart.
	Details []string

	// Refresh, with TabbedSpec.RefreshEvery, live-updates Items/Lines while
	// the surface is open (background MCP connects, incoming requests).
	Refresh func() []string
}

// TabbedSpec is a multi-tab surface: Tab switches panels, Enter commits ALL
// tabs (the /model questionnaire semantics), ESC/q cancels.
type TabbedSpec struct {
	Panels       []Panel
	RefreshEvery int64 // milliseconds; >0 with any Panel.Refresh enables live refresh
	// EnterAdvances turns Enter into wizard navigation: on any tab but the
	// last it moves to the NEXT tab, only the last tab's Enter commits. The
	// ask surfaces use it — unvisited questions must not be silently
	// submitted with defaults (the /model shape keeps commit-from-anywhere:
	// every tab there always holds a valid current value).
	EnterAdvances bool
}

// PanelResult is one panel's state at commit.
type PanelResult struct {
	Cursor  int      // List: highlighted index
	Checked []int    // Multi: checked indices (ascending)
	Value   *float64 // Slider
	On      bool     // Switch: final state
	Path    string   // Browser: chosen file ("" if none)
	Text    string   // Input: the submitted text
	Custom  string   // List/Multi with Custom: the inline "Other…" text
}

// TabbedResult reports the whole surface at commit.
type TabbedResult struct {
	Cancelled bool
	Focused   int // focused tab at Enter
	Panels    []PanelResult
}

// browseEntry is one row of the directory browser.
type browseEntry struct {
	name  string
	path  string
	isDir bool
}

// panelState is the mutable per-panel state.
type panelState struct {
	cursor  int
	checked map[int]bool
	value   *float64
	on      bool // Switch
	offset  int
	items   []string // live copy of Items/Lines (Refresh target)
	dir     string
	entries []browseEntry
	chosen  string
	errmsg  string
	copied  bool            // View: last "c" copied to clipboard
	wrapped int             // View(Wrap): wrapped row count from the last render
	hoff    int             // View(!Wrap): horizontal pan offset (h/l)
	input   textinput.Model // Input, and the inline Custom editor
	inOff   int             // input: horizontal window start column (see inputField)
	editing bool            // List/Multi Custom: the inline editor is open
	rows    int             // last visible row budget, for paging

	// Search (search.go). view maps a VISIBLE row to its underlying index and
	// is always populated — identity when no filter is on. cursor stays an
	// underlying index throughout (callers read it back that way), while
	// offset counts visible rows; every move goes through viewPos/setViewPos
	// so the two never disagree.
	search        searchState
	view          []int
	pendingCenter int   // View: logical line to centre at the next render (-1 = none)
	wrapStarts    []int // View(Wrap): logical line → first wrapped row, from the last render

	// Picker preview cache, keyed by what the render depends on.
	prevRows       []string
	prevIdx        int
	prevCols       int
	prevMaxRows    int
	prevCacheValid bool
}

// surfaceState drives an open Tabbed surface.
type surfaceState struct {
	spec  TabbedSpec
	focus int
	ps    []panelState
	reply chan TabbedResult
	gen   int // refresh-tick generation guard
}

func newSurface(spec TabbedSpec, reply chan TabbedResult, gen int) *surfaceState {
	s := &surfaceState{spec: spec, reply: reply, gen: gen}
	s.ps = make([]panelState, len(spec.Panels))
	for i, p := range spec.Panels {
		st := &s.ps[i]
		st.checked = map[int]bool{}
		st.pendingCenter = -1
		switch p.Kind {
		case PanelPicker:
			st.items = append([]string{}, p.Items...)
			st.cursor = p.Cursor
			if st.cursor < 0 || st.cursor >= len(st.items) {
				st.cursor = 0
			}
		case PanelList, PanelMulti:
			st.items = append([]string{}, p.Items...)
			if p.Custom {
				st.items = append(st.items, "Other…")
				ti := textinput.New()
				ti.Prompt = ""
				ti.SetVirtualCursor(false)
				ti.SetStyles(textinput.Styles{})
				st.input = ti
			}
			st.cursor = p.Cursor
			if st.cursor < 0 || st.cursor >= len(st.items) {
				st.cursor = 0
			}
			for _, c := range p.Checked {
				st.checked[c] = true
			}
		case PanelSlider:
			st.value = p.Value
		case PanelSwitch:
			st.on = p.On
		case PanelInput:
			ti := textinput.New()
			ti.Prompt = ""
			ti.Placeholder = p.Placeholder
			ti.SetVirtualCursor(false) // real terminal cursor: IME preedit anchors here
			// No internal SGR: the row's subtle-background wrapper styles the
			// whole box; lipgloss resets inside would punch holes in it.
			ti.SetStyles(textinput.Styles{})
			ti.SetValue(p.Text)
			if i == 0 {
				ti.Focus()
			}
			st.input = ti
		case PanelView:
			st.items = append([]string{}, p.Lines...)
		case PanelBrowser:
			dir := p.Dir
			if dir == "" {
				if wd, err := os.Getwd(); err == nil {
					dir = wd
				} else {
					dir, _ = os.UserHomeDir()
				}
			}
			st.setDir(p, dir)
		}
		st.rebuildView(p) // identity to start with; every render reads it
	}
	return s
}

// setDir loads a browser panel's directory ("../" + dirs + files, hidden
// entries skipped) — the v1 readDirItems semantics. Descending drops any
// filter: the query was aimed at the directory being left.
func (st *panelState) setDir(p Panel, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		st.errmsg = err.Error()
		return
	}
	st.dir = dir
	st.errmsg = ""
	st.entries = nil
	if parent := filepath.Dir(dir); parent != dir {
		st.entries = append(st.entries, browseEntry{"../", parent, true})
	}
	var dirs, files []browseEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(dir, name)
		if e.IsDir() {
			dirs = append(dirs, browseEntry{name + "/", p, true})
		} else {
			files = append(files, browseEntry{name, p, false})
		}
	}
	st.entries = append(st.entries, dirs...)
	st.entries = append(st.entries, files...)
	st.cursor = 0
	st.offset = 0
	st.searchClear(p)
}

// result collects the commit-all snapshot.
func (s *surfaceState) result() TabbedResult {
	r := TabbedResult{Focused: s.focus, Panels: make([]PanelResult, len(s.ps))}
	for i := range s.ps {
		st := &s.ps[i]
		pr := PanelResult{Cursor: st.cursor, Value: st.value, On: st.on, Path: st.chosen, Text: st.input.Value()}
		if s.spec.Panels[i].Custom {
			pr.Custom = strings.TrimSpace(st.input.Value())
			pr.Text = ""
		}
		for j := 0; j < len(st.items); j++ {
			if st.checked[j] {
				pr.Checked = append(pr.Checked, j)
			}
		}
		r.Panels[i] = pr
	}
	return r
}

// setFocus moves the focused tab, keeping exactly the focused input panel's
// field focused (focus drives both editing and the real-cursor position).
func (s *surfaceState) setFocus(i int) {
	if s.spec.Panels[s.focus].Kind == PanelInput {
		s.ps[s.focus].input.Blur()
	}
	if s.ps[s.focus].editing {
		s.ps[s.focus].editing = false
		s.ps[s.focus].input.Blur()
	}
	// A half-typed query is abandoned on the way out (an APPLIED one stays —
	// per-panel search survives tabbing away and back), so the field never
	// holds the cursor from an unfocused tab.
	if s.ps[s.focus].search.mode == searchTyping {
		s.ps[s.focus].searchClear(s.spec.Panels[s.focus])
	}
	s.focus = i
	if s.spec.Panels[s.focus].Kind == PanelInput {
		s.ps[s.focus].input.Focus()
	}
}

// inputCursorCols converts the field cursor's RUNE index (what
// textinput.Cursor reports as X) into display columns: wide (CJK) runes
// occupy two columns, so the raw index drifts the real terminal cursor one
// column left per wide rune before it. The result is an ABSOLUTE column in
// the full value — inputField turns it into a column inside the visible
// window.
func inputCursorCols(ti textinput.Model, runeIdx int) int {
	runes := []rune(ti.Value())
	if runeIdx > len(runes) {
		runeIdx = len(runes)
	}
	if runeIdx < 0 {
		runeIdx = 0
	}
	return textwidth.StringWidth(string(runes[:runeIdx]))
}

// inputField renders a text field clipped to boxW columns and reports where
// the cursor sits INSIDE that window, panning *off as needed to keep the
// cursor visible.
//
// The field's own horizontal scrolling is deliberately switched off —
// SetWidth(0), which bubbles reads as "no viewport", so View() returns the
// whole value. Letting it scroll instead is what used to misplace the
// cursor: its offset is unexported, and its Cursor() reports an absolute
// position that ignores the scroll, so any value longer than the box parked
// the real terminal cursor outside the field (typically pinned to the right
// edge, or past it). Owning the window here keeps ONE model of what is
// visible, and the cursor column is derived from that same model.
//
// The rendered view already ends in the cursor's own cell (textinput draws a
// blank there), so the value's last column and the cursor both fit inside the
// box.
func inputField(ti *textinput.Model, off *int, boxW int) (string, int) {
	ti.SetWidth(0)
	view := ti.View()
	if boxW < 1 {
		boxW = 1
	}
	cur := 0
	if c := ti.Cursor(); c != nil {
		cur = inputCursorCols(*ti, c.X)
	}
	total := ansi.StringWidth(view)
	if total <= boxW {
		*off = 0 // fits, cursor cell included: never pan
	} else {
		if *off > cur {
			*off = cur
		}
		if edge := cur - (boxW - 1); *off < edge {
			*off = edge
		}
		// Don't pan past the end — trailing blanks inside the box read as a
		// rendering bug when text is sitting off to the left.
		if max := total - boxW; *off > max {
			*off = max
		}
		if *off < 0 {
			*off = 0
		}
	}
	// A window that starts mid-glyph (wide runes are two columns) would make
	// ansi.Cut keep the whole glyph and overflow the box: step right until it
	// lands on a boundary. The cursor column follows the window, so it stays
	// inside either way.
	field := ansi.Cut(view, *off, *off+boxW)
	for ansi.StringWidth(field) > boxW && *off < total {
		*off++
		field = ansi.Cut(view, *off, *off+boxW)
	}
	return field, cur - *off
}

// sliderStep ports the v1 SliderPanel semantics: integer step indices (no
// float accumulation), default↔range transitions, Max clamp.
func sliderStep(st *panelState, p Panel, dir int) {
	if st.value == nil {
		if dir > 0 {
			v := p.Min
			st.value = &v
		}
		return
	}
	steps := math.Round((*st.value-p.Min)/p.Step) + float64(dir)
	v := p.Min + steps*p.Step
	v = math.Round(v*1e9) / 1e9
	if v < p.Min {
		st.value = nil
		return
	}
	if v > p.Max {
		v = p.Max
	}
	st.value = &v
}

// panelHeight is a panel's visible row budget.
func panelHeight(p Panel, n int) int {
	h := p.Height
	if h <= 0 {
		h = 10
		if p.Kind == PanelView {
			h = 15
		}
	}
	if n < h {
		h = n
	}
	if h < 1 {
		h = 1
	}
	return h
}

// renderSurface draws the tab bar, the focused panel, and a help hint.
func (m *model) renderSurface(b *strings.Builder) {
	s := m.surf
	w := maxInt(4, m.width)

	// Tab bar (multi-panel only).
	if len(s.spec.Panels) > 1 {
		var bar strings.Builder
		for i, p := range s.spec.Panels {
			if i > 0 {
				bar.WriteString(faint + " │ " + sgrReset)
			}
			// Both states carry identical " title " padding so switching
			// focus never changes the bar width (only the styling swaps).
			if i == s.focus {
				bar.WriteString(revOn + " " + p.Title + " " + sgrReset)
			} else {
				bar.WriteString(faint + " " + p.Title + " " + sgrReset)
			}
		}
		b.WriteString("\n" + ansi.Truncate(bar.String(), w, "…"))
	} else {
		// Single-panel surfaces reuse the focused-chip style so titles read
		// the same with or without tabs.
		b.WriteString("\n" + revOn + " " + s.spec.Panels[0].Title + " " + sgrReset)
	}

	p := s.spec.Panels[s.focus]
	st := &s.ps[s.focus]

	if p.Prompt != "" {
		b.WriteString("\n" + faint + " " + ansi.Truncate(p.Prompt, maxInt(4, w-2), "…") + sgrReset)
	}

	switch p.Kind {
	case PanelPicker:
		m.renderPicker(b, p, st, w)
	case PanelList, PanelMulti:
		h := panelHeight(p, len(st.view))
		st.rows = h
		st.scrollTo(h)
		for vp := st.offset; vp < st.offset+h && vp < len(st.view); vp++ {
			i := st.view[vp]
			marker := "  "
			if i == st.cursor {
				marker = cyan + "▸ " + sgrReset
			}
			box := ""
			if p.Kind == PanelMulti {
				box = faint + "[ ] " + sgrReset
				if st.checked[i] {
					box = green + "[x] " + sgrReset
				}
			}
			if p.Custom && i == len(p.Items) {
				// The "Other…" row: an inline editor while editing, the saved
				// text once entered, the plain affordance otherwise.
				if st.editing {
					prefixCols := 2
					if p.Kind == PanelMulti {
						prefixCols += 4
					}
					boxW := clampInt(40, 4, maxInt(4, w-prefixCols-4))
					field, curCol := inputField(&st.input, &st.inOff, boxW)
					if st.input.Value() == "" {
						field = faint + ansi.Truncate("your answer", boxW, "…")
					}
					pad := boxW - ansi.StringWidth(field)
					if pad < 0 {
						pad = 0
					}
					if c := st.input.Cursor(); c != nil {
						m.surfCur = c
						m.surfCur.Y = strings.Count(b.String(), "\n") + 1
						m.surfCur.X = prefixCols + curCol
					}
					// No leading padding column: the field's first text cell
					// sits exactly where the option labels start, so the box
					// reads as one of the rows (only a trailing pad closes
					// the shade).
					bg := inputBg()
					b.WriteString("\n" + marker + box + bg + field + sgrReset + bg +
						strings.Repeat(" ", pad) + " " + sgrReset)
					continue
				}
				if v := strings.TrimSpace(st.input.Value()); v != "" {
					st.items[i] = "Other: " + v
				} else {
					st.items[i] = "Other…"
				}
			}
			item := ansi.Truncate(st.items[i], maxInt(4, w-6), "…")
			// v1 highlight semantics: the cursor row's TEXT goes cyan when the
			// row is plain; rows carrying their own ANSI styling (e.g. /debug
			// request rows) keep it, with just the cyan marker.
			if i == st.cursor && !strings.Contains(item, "\x1b[") {
				item = cyan + item + sgrReset
			}
			b.WriteString("\n" + marker + box + item)
		}
	case PanelSlider:
		// Right-align the value label to "default"'s width so toggling
		// between default and a number never shifts the bar's origin.
		label := "default"
		pct := 0.0
		if st.value != nil {
			label = fmt.Sprintf("%.1f", *st.value)
			if p.Max > p.Min {
				pct = (*st.value - p.Min) / (p.Max - p.Min)
			}
		}
		val := fmt.Sprintf("%7s", label)
		if st.value == nil {
			val = faint + val + sgrReset
		}
		bar := progress.New(progress.WithoutPercentage(), progress.WithColors(lipgloss.Color("6")))
		bar.SetWidth(clampInt(w-13, 10, 40))
		// Blank rows above and below give the lone bar row some breathing room.
		b.WriteString("\n\n  " + val + "  " + bar.ViewAs(pct) + "\n")
	case PanelSwitch:
		// The slider's row geometry: identical widths in both states
		// (right-aligned label, fixed-length track) so toggling never shifts
		// the layout — the knob slides, the colors swap.
		const track = 6
		label, toggle := "Off", faint+"●"+strings.Repeat("─", track)+sgrReset
		if st.on {
			label, toggle = "On", green+strings.Repeat("━", track)+"●"+sgrReset
		}
		val := fmt.Sprintf("%3s", label)
		if !st.on {
			val = faint + val + sgrReset
		}
		b.WriteString("\n\n  " + val + "  " + toggle + "\n")
	case PanelInput:
		boxW := p.InputWidth
		if boxW <= 0 {
			boxW = 40
		}
		boxW = clampInt(boxW, 4, maxInt(4, w-6))
		view, curCol := inputField(&st.input, &st.inOff, boxW)
		if st.input.Value() == "" && p.Placeholder != "" {
			// Our own placeholder render: faint on the field background
			// (textinput's would come unstyled — indistinguishable from
			// typed text).
			view = faint + ansi.Truncate(p.Placeholder, boxW, "…")
		}
		pad := boxW - ansi.StringWidth(view)
		if pad < 0 {
			pad = 0
		}
		// Track where the field's cursor lands so View can place the REAL
		// terminal cursor there (rows written so far + the blank row + this
		// row; columns: 2 indent + 1 box padding).
		if c := st.input.Cursor(); c != nil {
			m.surfCur = c
			m.surfCur.Y = strings.Count(b.String(), "\n") + 2
			m.surfCur.X = 3 + curCol
		}
		// Blank rows above and below, like the slider. Text renders in the
		// default foreground on the adaptive background shade; the trailing
		// re-assert keeps the padding shaded even if the field content ever
		// carries a reset.
		bg := inputBg()
		b.WriteString("\n\n  " + bg + " " + view + sgrReset + bg +
			strings.Repeat(" ", pad) + " " + sgrReset + "\n")
	case PanelView:
		lines := st.items
		// Highlight BEFORE wrapping: hits are recorded against logical lines,
		// and wrapANSI carries the SGR across row boundaries, so a match split
		// by a wrap stays highlighted on both rows.
		if q := st.searchDraft(); q != "" {
			cur := st.currentHit()
			hl := make([]string, len(lines))
			for i, l := range lines {
				off := -1
				if cur != nil && cur.line == i {
					off = cur.off
				}
				hl[i] = highlightLine(l, q, off)
			}
			lines = hl
		}
		if p.Wrap {
			// Remember where each logical line begins so a hit can be centred
			// in wrapped-row space, which is the only space offset speaks.
			starts := make([]int, len(lines))
			var rows []string
			for i, l := range lines {
				starts[i] = len(rows)
				rows = append(rows, wrapANSI(l, w)...)
			}
			lines, st.wrapStarts = rows, starts
		}
		st.wrapped = len(lines)
		h := panelHeight(p, len(lines))
		st.rows = h
		maxOff := len(lines) - h
		if maxOff < 0 {
			maxOff = 0
		}
		// A pending jump lands here, where the wrapped geometry is finally
		// known — the same "key sets the intent, render resolves it" shape the
		// G key uses with its 1<<30 offset.
		if st.pendingCenter >= 0 {
			row := st.pendingCenter
			if p.Wrap && row < len(st.wrapStarts) {
				row = st.wrapStarts[row]
			}
			st.offset = row - h/2
			st.pendingCenter = -1
		}
		st.offset = clampInt(st.offset, 0, maxOff)
		if st.hoff < 0 || p.Wrap {
			st.hoff = 0
		}
		for _, l := range lines[st.offset : st.offset+h] {
			if st.hoff > 0 {
				b.WriteString("\n" + clipLine(l, st.hoff, w))
			} else {
				b.WriteString("\n" + ansi.Truncate(l, w, "…"))
			}
		}
	case PanelBrowser:
		b.WriteString("\n" + faint + ansi.Truncate(st.dir, w, "…") + sgrReset)
		if st.errmsg != "" {
			b.WriteString("\n" + ErrPrefix + st.errmsg)
		}
		h := panelHeight(p, len(st.view))
		st.rows = h
		st.scrollTo(h)
		for vp := st.offset; vp < st.offset+h && vp < len(st.view); vp++ {
			i := st.view[vp]
			marker := "  "
			if i == st.cursor {
				marker = cyan + "▸ " + sgrReset
			}
			name := ansi.Truncate(st.entries[i].name, maxInt(4, w-4), "…")
			switch {
			case i == st.cursor:
				name = cyan + name + sgrReset
			case st.entries[i].isDir:
				name = faint + name + sgrReset
			}
			b.WriteString("\n" + marker + name)
		}
	}

	// The query field REPLACES the hint row rather than adding one: both are a
	// single row, so the frame's height is identical in and out of search and
	// nothing below it moves.
	if st.search.mode == searchTyping {
		m.renderSearchRow(b, p, st, w)
		return
	}

	hint := surfaceHint(p, st)
	if len(s.spec.Panels) > 1 {
		hint = "Tab switch · " + hint
	}
	if pct, ok := scrollPercent(p, st); ok {
		hint = fmt.Sprintf("%s · %d%%", hint, pct)
	}
	b.WriteString("\n" + faint + ansi.Truncate(hint, maxInt(4, m.width), "…") + sgrReset)
}

// renderSearchRow draws the query field in the hint row's place, with the
// live match count trailing it, and parks the REAL terminal cursor in the
// field (IME preedit anchors to it, as in the Input panel).
func (m *model) renderSearchRow(b *strings.Builder, p Panel, st *panelState, w int) {
	ti := &st.search.input
	boxW := clampInt(32, 4, maxInt(4, w-24))
	field, curCol := inputField(ti, &st.search.inOff, boxW)
	if c := ti.Cursor(); c != nil {
		m.surfCur = c
		m.surfCur.Y = strings.Count(b.String(), "\n") + 1
		m.surfCur.X = 1 + curCol
	}
	b.WriteString("\n" + cyan + "/" + sgrReset + field +
		faint + " · " + searchTypingHint(p, st) + sgrReset)
}

// searchTypingHint is the live feedback beside the query: how much the filter
// keeps, or how many hits a View has, plus the keys.
func searchTypingHint(p Panel, st *panelState) string {
	q := st.searchDraft()
	switch {
	case q == "":
		return "type to search · Esc cancel"
	case p.Kind == PanelView:
		if n := len(st.search.hits); n > 0 {
			return fmt.Sprintf("%d hits · Enter search · Esc cancel", n)
		}
		return "no match · Esc cancel"
	default:
		total := st.rowCount(p)
		if shown, ok := st.filteredCount(p); ok {
			return fmt.Sprintf("%d of %d · Enter filter · Esc cancel", shown, total)
		}
		return "no match · Esc cancel"
	}
}

// Picker geometry: the preview claims a share of the width, capped, and only
// when the terminal is wide enough for both columns to stay readable.
const (
	pickerMinWidth   = 64 // below this the preview is dropped entirely
	pickerPreviewCap = 44 // preview columns never exceed this
	pickerGutter     = 2
	pickerMinList    = 18 // list columns the preview must never eat into
	pickerRows       = 14 // preview rows (Panel.Height overrides), terminal permitting
	pickerFrameRows  = 12 // frame rows the preview must leave for everything else
)

// pickerPreviewCols returns the preview pane's width (0 = no preview).
func pickerPreviewCols(p Panel, w int) int {
	if p.Preview == nil || w < pickerMinWidth {
		return 0
	}
	cols := w * 2 / 5
	if cols > pickerPreviewCap {
		cols = pickerPreviewCap
	}
	if w-cols-pickerGutter < pickerMinList {
		cols = w - pickerGutter - pickerMinList
	}
	if cols < 8 {
		return 0
	}
	return cols
}

// renderPicker draws the preview pane beside the item list, then the current
// item's detail line. Rows are composed cell by cell: every preview row is
// padded to the pane width by DISPLAY width (internal/textwidth), so the list
// column starts at the same screen column on every row regardless of the SGR
// the preview carries.
func (m *model) renderPicker(b *strings.Builder, p Panel, st *panelState, w int) {
	// The list window follows the item count; the preview gets its own row
	// budget (a 3-item list must still show a legible thumbnail), clamped so
	// the inline frame keeps room for the composer and status row.
	h := panelHeight(p, len(st.view))
	st.rows = h
	st.scrollTo(h)

	var preview []string
	if cols := pickerPreviewCols(p, w); cols > 0 && len(st.items) > 0 {
		prevH := p.Height
		if prevH <= 0 {
			prevH = pickerRows
		}
		if m.height > 0 {
			prevH = clampInt(prevH, 4, maxInt(4, m.height-pickerFrameRows))
		}
		preview = st.previewRows(p, cols, prevH)
	}

	// Columns hug the RENDERED preview, not the reserved pane: a portrait
	// thumbnail bounded by height comes out far narrower than its budget, and
	// padding to the budget would open a gulf between picture and list. Every
	// row pads to the same measured width, so the list column stays put.
	previewW := 0
	for _, row := range preview {
		if rw := ansi.StringWidth(row); rw > previewW {
			previewW = rw
		}
	}
	listCols := w - 2 // the "▸ " marker gutter
	if previewW > 0 {
		listCols = w - previewW - pickerGutter - 2
	}
	listCols = maxInt(4, listCols)

	rows := h
	if len(preview) > rows {
		rows = len(preview)
	}
	for r := 0; r < rows; r++ {
		line := ""
		if previewW > 0 {
			cell := ""
			if r < len(preview) {
				cell = preview[r]
			}
			// ANSI-aware width: preview rows are dense with SGR, and counting
			// those bytes as glyphs would collapse the pad to zero and let
			// the list column drift row by row.
			pad := previewW - ansi.StringWidth(cell)
			if pad < 0 {
				pad = 0
			}
			line = cell + strings.Repeat(" ", pad) + strings.Repeat(" ", pickerGutter)
		}
		if vp := st.offset + r; r < h && vp < len(st.view) {
			i := st.view[vp]
			marker := "  "
			item := ansi.Truncate(st.items[i], listCols, "…")
			if i == st.cursor {
				marker = cyan + "▸ " + sgrReset
				if !strings.Contains(item, "\x1b[") {
					item = cyan + item + sgrReset
				}
			}
			line += marker + item
		}
		b.WriteString("\n" + strings.TrimRight(line, " "))
	}

	// The detail line follows the cursor: a caller-sized string (an OSC 8
	// path link here) printed verbatim — see Panel.Details.
	if st.cursor >= 0 && st.cursor < len(p.Details) {
		if d := p.Details[st.cursor]; d != "" {
			b.WriteString("\n" + faint + d + sgrReset)
		}
	}
}

// previewRows returns the highlighted item's preview, rendering it only when
// the selection or the pane geometry has changed since the last frame.
func (st *panelState) previewRows(p Panel, cols, rows int) []string {
	if st.prevCacheValid && st.prevIdx == st.cursor && st.prevCols == cols && st.prevMaxRows == rows {
		return st.prevRows
	}
	st.prevRows = p.Preview(st.cursor, cols, rows)
	st.prevIdx, st.prevCols, st.prevMaxRows, st.prevCacheValid = st.cursor, cols, rows, true
	return st.prevRows
}

// surfaceHint mirrors the v1 promptui help lines exactly, plus whatever an
// applied search adds: a row panel keeps ALL its keys and gains "c clear"; a
// View swaps in the n/p walker until Esc puts the query back up.
//
// The View walker's hint lists only the SEARCH keys. Its panel keys — scroll,
// g/G, c copy — all still work, but none of them are advertised while the
// search is on; the row is for the mode you are in, and q/Esc is one keystroke
// from the full list.
func surfaceHint(p Panel, st *panelState) string {
	if st.search.mode == searchApplied {
		q := truncateQuery(st.search.query)
		if p.Kind == PanelView {
			if n := len(st.search.hits); n > 0 {
				return fmt.Sprintf("%s %d/%d · n next · p prev · q/Esc edit", q, st.search.hitIdx+1, n)
			}
			return q + " no match · q/Esc edit"
		}
		shown, ok := st.filteredCount(p)
		lead := fmt.Sprintf("%s %d/%d", q, shown, st.rowCount(p))
		if !ok {
			lead = q + " no match"
		}
		return lead + " · " + baseHint(p, st) + " · c clear"
	}
	if st.searchAvailable(p) {
		return baseHint(p, st) + " · / search"
	}
	return baseHint(p, st)
}

// truncateQuery renders a query for the hint row, clipped so a long one cannot
// crowd out the keys behind it.
func truncateQuery(q string) string {
	const max = 24
	r := []rune(q)
	if len(r) > max {
		q = string(r[:max]) + "…"
	}
	return "\"" + q + "\""
}

func baseHint(p Panel, st *panelState) string {
	switch p.Kind {
	case PanelSlider:
		return "←→ adjust · g default · G max · Enter confirm · q/Esc cancel"
	case PanelSwitch:
		return "Space toggle · ←→ off/on · Enter confirm · q/Esc cancel"
	case PanelInput:
		return "←→ move · Enter confirm · Esc cancel"
	case PanelPicker:
		return "↑↓ move · ←→ page · Enter confirm · q/Esc cancel"
	case PanelList, PanelMulti:
		if st.editing {
			return "Enter confirm · Esc back to options"
		}
		if p.Kind == PanelMulti {
			return "↑↓ move · ←→ page · Space toggle · Enter confirm · q/Esc cancel"
		}
		return "↑↓ move · ←→ page · Enter confirm · q/Esc cancel"
	case PanelView:
		if st.copied {
			return "✓ copied to clipboard"
		}
		if p.Wrap {
			return "↑↓ scroll · ←→ page · g/G top/bottom · c copy · q/Esc close"
		}
		return "↑↓ scroll · ←→ page · h/l pan · g/G top/bottom · c copy · q/Esc close"
	case PanelBrowser:
		return "↑↓ move · ←→ page · Enter open/choose · g/G top/bottom · q/Esc cancel"
	default:
		return "↑↓ move · ←→ page · Space toggle · Enter confirm · q/Esc cancel"
	}
}

// scrollPercent mirrors v1: how far through a scrollable panel we are. Row
// panels measure progress through what is VISIBLE, so a filter's percentage
// describes the filtered list rather than the hidden whole.
func scrollPercent(p Panel, st *panelState) (int, bool) {
	switch p.Kind {
	case PanelList, PanelMulti, PanelPicker, PanelBrowser:
		n := len(st.view)
		if h := panelHeight(p, n); n > h && n > 1 {
			return int(float64(st.viewPos())/float64(n-1)*100 + 0.5), true
		}
	case PanelView:
		n := len(st.items)
		if p.Wrap {
			n = st.wrapped // set at render
		}
		h := panelHeight(p, n)
		if maxOff := n - h; maxOff > 0 {
			off := clampInt(st.offset, 0, maxOff)
			return int(float64(off)/float64(maxOff)*100 + 0.5), true
		}
	}
	return 0, false
}

// scrollTo keeps the cursor visible in an h-row window. Both coordinates are
// VISIBLE rows: offset counts them, and the cursor (an underlying index) is
// translated through the view mapping.
func (st *panelState) scrollTo(h int) {
	n := len(st.view)
	vp := st.viewPos()
	if vp < st.offset {
		st.offset = vp
	}
	if vp >= st.offset+h {
		st.offset = vp - h + 1
	}
	if st.offset < 0 {
		st.offset = 0
	}
	if st.offset > n-1 {
		st.offset = maxInt(0, n-1)
	}
}
