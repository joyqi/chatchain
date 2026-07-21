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

	// Height caps the panel's visible rows (0 = default).
	Height int

	// Browser starting directory ("" = working directory).
	Dir string

	// Input: a one-line text field with a subtle background; content beyond
	// InputWidth scrolls horizontally. Text is the initial value.
	Text        string
	Placeholder string
	InputWidth  int // visible columns (0 = 40), clamped to the terminal

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
	editing bool            // List/Multi Custom: the inline editor is open
	rows    int             // last visible row budget, for paging
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
		switch p.Kind {
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
			st.setDir(dir)
		}
	}
	return s
}

// setDir loads a browser panel's directory ("../" + dirs + files, hidden
// entries skipped) — the v1 readDirItems semantics.
func (st *panelState) setDir(dir string) {
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
	s.focus = i
	if s.spec.Panels[s.focus].Kind == PanelInput {
		s.ps[s.focus].input.Focus()
	}
}

// inputCursorCols converts the field cursor's RUNE index (what
// textinput.Cursor reports as X) into display columns: wide (CJK) runes
// occupy two columns, so the raw index drifts the real terminal cursor one
// column left per wide rune before it. Horizontal scroll is not modeled —
// content longer than the box already approximates.
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
	case PanelList, PanelMulti:
		h := panelHeight(p, len(st.items))
		st.rows = h
		st.scrollTo(h)
		for i := st.offset; i < st.offset+h && i < len(st.items); i++ {
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
					st.input.SetWidth(boxW)
					field := st.input.View()
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
						m.surfCur.X = prefixCols + inputCursorCols(st.input, c.X)
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
		st.input.SetWidth(boxW)
		view := st.input.View()
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
			m.surfCur.X = 3 + inputCursorCols(st.input, c.X)
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
		if p.Wrap {
			lines = nil
			for _, l := range st.items {
				lines = append(lines, wrapANSI(l, w)...)
			}
		}
		st.wrapped = len(lines)
		h := panelHeight(p, len(lines))
		st.rows = h
		maxOff := len(lines) - h
		if maxOff < 0 {
			maxOff = 0
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
		h := panelHeight(p, len(st.entries))
		st.rows = h
		st.scrollToN(h, len(st.entries))
		for i := st.offset; i < st.offset+h && i < len(st.entries); i++ {
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

	hint := surfaceHint(p, st)
	if len(s.spec.Panels) > 1 {
		hint = "Tab switch · " + hint
	}
	if pct, ok := scrollPercent(p, st); ok {
		hint = fmt.Sprintf("%s · %d%%", hint, pct)
	}
	b.WriteString("\n" + faint + ansi.Truncate(hint, maxInt(4, m.width), "…") + sgrReset)
}

// surfaceHint mirrors the v1 promptui help lines exactly.
func surfaceHint(p Panel, st *panelState) string {
	switch p.Kind {
	case PanelSlider:
		return "←→ adjust · g default · G max · Enter confirm · q/Esc cancel"
	case PanelSwitch:
		return "Space toggle · ←→ off/on · Enter confirm · q/Esc cancel"
	case PanelInput:
		return "←→ move · Enter confirm · Esc cancel"
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

// scrollPercent mirrors v1: how far through a scrollable panel we are.
func scrollPercent(p Panel, st *panelState) (int, bool) {
	switch p.Kind {
	case PanelList, PanelMulti:
		n := len(st.items)
		if h := panelHeight(p, n); n > h && n > 1 {
			return int(float64(st.cursor)/float64(n-1)*100 + 0.5), true
		}
	case PanelBrowser:
		n := len(st.entries)
		if h := panelHeight(p, n); n > h && n > 1 {
			return int(float64(st.cursor)/float64(n-1)*100 + 0.5), true
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

// scrollTo keeps the cursor visible in an h-row window over items.
func (st *panelState) scrollTo(h int) { st.scrollToN(h, len(st.items)) }

func (st *panelState) scrollToN(h, n int) {
	if st.cursor < st.offset {
		st.offset = st.cursor
	}
	if st.cursor >= st.offset+h {
		st.offset = st.cursor - h + 1
	}
	if st.offset < 0 {
		st.offset = 0
	}
	if st.offset > n-1 {
		st.offset = maxInt(0, n-1)
	}
}
