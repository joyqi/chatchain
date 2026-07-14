package ui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// PanelKind selects a tabbed panel's behavior — the v2 ports of the vendored
// promptui panels (list, checkbox multi-select, slider, directory browser,
// read-only view).
type PanelKind int

const (
	PanelList PanelKind = iota
	PanelMulti
	PanelSlider
	PanelBrowser
	PanelView
)

// Panel describes one tab of a Tabbed surface.
type Panel struct {
	Title string
	Kind  PanelKind

	// List / Multi.
	Items   []string
	Cursor  int   // initial cursor (List)
	Checked []int // initially checked indices (Multi)

	// Slider (←/→ step, g default, G max).
	Min, Max, Step float64
	Value          *float64 // nil = default

	// View.
	Lines []string
	Wrap  bool

	// Height caps the panel's visible rows (0 = default).
	Height int

	// Browser starting directory ("" = working directory).
	Dir string

	// Refresh, with TabbedSpec.RefreshEvery, live-updates Items/Lines while
	// the surface is open (background MCP connects, incoming requests).
	Refresh func() []string
}

// TabbedSpec is a multi-tab surface: Tab switches panels, Enter commits ALL
// tabs (the /model questionnaire semantics), ESC/q cancels.
type TabbedSpec struct {
	Panels       []Panel
	RefreshEvery int64 // milliseconds; >0 with any Panel.Refresh enables live refresh
}

// PanelResult is one panel's state at commit.
type PanelResult struct {
	Cursor  int      // List: highlighted index
	Checked []int    // Multi: checked indices (ascending)
	Value   *float64 // Slider
	Path    string   // Browser: chosen file ("" if none)
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
	offset  int
	items   []string // live copy of Items/Lines (Refresh target)
	dir     string
	entries []browseEntry
	chosen  string
	errmsg  string
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
			st.cursor = p.Cursor
			if st.cursor < 0 || st.cursor >= len(st.items) {
				st.cursor = 0
			}
			for _, c := range p.Checked {
				st.checked[c] = true
			}
		case PanelSlider:
			st.value = p.Value
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
		pr := PanelResult{Cursor: st.cursor, Value: st.value, Path: st.chosen}
		for j := 0; j < len(st.items); j++ {
			if st.checked[j] {
				pr.Checked = append(pr.Checked, j)
			}
		}
		r.Panels[i] = pr
	}
	return r
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
			if i == s.focus {
				bar.WriteString(revOn + " " + p.Title + " " + sgrReset)
			} else {
				bar.WriteString(faint + p.Title + sgrReset)
			}
		}
		b.WriteString("\n" + ansi.Truncate(bar.String(), w, "…"))
	} else {
		b.WriteString("\n" + faint + "── " + s.spec.Panels[0].Title + " ──" + sgrReset)
	}

	p := s.spec.Panels[s.focus]
	st := &s.ps[s.focus]

	switch p.Kind {
	case PanelList, PanelMulti:
		h := panelHeight(p, len(st.items))
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
			b.WriteString("\n" + marker + box + ansi.Truncate(st.items[i], maxInt(4, w-6), "…"))
		}
	case PanelSlider:
		val := faint + "default" + sgrReset
		if st.value != nil {
			val = fmt.Sprintf("%.1f", *st.value)
		}
		b.WriteString("\n  " + val + "  " + sliderBar(st.value, p, maxInt(10, w-16)))
	case PanelView:
		lines := st.items
		if p.Wrap {
			lines = nil
			for _, l := range st.items {
				lines = append(lines, wrapByWidth(l, w)...)
			}
		}
		h := panelHeight(p, len(lines))
		maxOff := len(lines) - h
		if maxOff < 0 {
			maxOff = 0
		}
		if st.offset > maxOff {
			st.offset = maxOff
		}
		for _, l := range lines[st.offset : st.offset+h] {
			b.WriteString("\n" + ansi.Truncate(l, w, "…"))
		}
	case PanelBrowser:
		b.WriteString("\n" + faint + ansi.Truncate(st.dir, w, "…") + sgrReset)
		if st.errmsg != "" {
			b.WriteString("\n" + ErrPrefix + st.errmsg)
		}
		h := panelHeight(p, len(st.entries))
		st.scrollToN(h, len(st.entries))
		for i := st.offset; i < st.offset+h && i < len(st.entries); i++ {
			marker := "  "
			if i == st.cursor {
				marker = cyan + "▸ " + sgrReset
			}
			name := st.entries[i].name
			if st.entries[i].isDir {
				name = faint + name + sgrReset
			}
			b.WriteString("\n" + marker + ansi.Truncate(name, maxInt(4, w-4), "…"))
		}
	}

	b.WriteString("\n" + faint + surfaceHint(p.Kind, len(s.spec.Panels) > 1) + sgrReset)
}

// ErrPrefix styles surface-level error rows.
const ErrPrefix = "\x1b[31m⚠ \x1b[0m"

func surfaceHint(k PanelKind, multi bool) string {
	h := ""
	if multi {
		h = "Tab 切换 · "
	}
	switch k {
	case PanelMulti:
		return h + "Space 勾选 · Enter 提交 · ESC 取消"
	case PanelSlider:
		return h + "←→ 调整 · g 默认 · Enter 提交 · ESC 取消"
	case PanelBrowser:
		return h + "Enter 进入/选择 · ESC 取消"
	case PanelView:
		return h + "↑↓ 滚动 · q 关闭"
	default:
		return h + "↑↓ · Enter 提交 · ESC 取消"
	}
}

// sliderBar renders a coarse position bar.
func sliderBar(v *float64, p Panel, width int) string {
	pos := -1
	if v != nil && p.Max > p.Min {
		pos = int((*v - p.Min) / (p.Max - p.Min) * float64(width-1))
	}
	var bar strings.Builder
	bar.WriteString(faint)
	for i := 0; i < width; i++ {
		if i == pos {
			bar.WriteString(sgrReset + cyan + "●" + sgrReset + faint)
		} else {
			bar.WriteString("─")
		}
	}
	bar.WriteString(sgrReset)
	return bar.String()
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
