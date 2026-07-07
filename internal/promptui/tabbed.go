package promptui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"chatchain/internal/promptui/screenbuf"
	"chatchain/internal/readline"
)

// Panel is one tab inside a Tabbed selector. The container owns the terminal
// loop; a panel is a passive object that only renders its body and reacts to
// keys. This mirrors Select's inner loop but factors rendering out so several
// panels can share a single readline instance.
type Panel interface {
	// Title is the tab label shown in the tab bar.
	Title() string
	// Render returns the panel body's lines (already fitted to width, may carry
	// ANSI); height is the number of body rows available to the panel.
	Render(width, height int) []string
	// HandleKey processes one key; consumed=true means the panel handled it and
	// the container should not interpret it further.
	HandleKey(key rune) (consumed bool)
}

// Tabbed shows several Panels behind a tab bar and drives them from a single
// readline loop, repainting in place via screenbuf — the reusable core behind
// the merged /file and /session commands. Selection state lives in the panels;
// callers read it after Run returns (the focused tab for a command, every tab
// for a questionnaire).
type Tabbed struct {
	// Panels are the tabs, left to right. At least one is required.
	Panels []Panel
	// RuneWidth returns a rune's display width (CJK = 2); nil means 1 per rune.
	RuneWidth func(rune) int
	// Stdin is the input source; defaults to os.Stdin (injected in tests).
	Stdin io.ReadCloser
	// Stdout is the output sink; defaults to os.Stdout.
	Stdout io.WriteCloser
	// Size caps the number of body rows shown before a panel scrolls. Defaults
	// to 15.
	Size int

	focused int
}

// Run displays the selector and blocks until the user submits (Enter on a
// non-consuming panel) or cancels (Ctrl+C / Esc → ErrInterrupt). It returns the
// index of the tab focused at submission time; the selection itself stays in the
// panel objects for the caller to read.
func (t *Tabbed) Run() (int, error) {
	if len(t.Panels) == 0 {
		return 0, fmt.Errorf("tabbed: no panels")
	}
	size := t.Size
	if size <= 0 {
		size = 15
	}

	c := &readline.Config{
		Stdin:         t.Stdin,
		Stdout:        t.Stdout,
		EscapeCancels: true, // standalone ESC cancels the selector natively
	}
	if err := c.Init(); err != nil {
		return 0, err
	}
	c.HistoryLimit = -1
	c.UniqueEditLine = true

	// q quits (like Ctrl+C / Esc), matching Select. Handled via the input filter
	// rather than the listener because the listener cannot end the run.
	c.FuncFilterInputRune = func(r rune) (rune, bool) {
		if r == 'q' {
			return readline.CharInterrupt, true
		}
		return r, true
	}

	rl, err := readline.NewEx(c)
	if err != nil {
		return 0, err
	}

	rl.Write([]byte(hideCursor))
	sb := screenbuf.New(rl)

	width, _ := c.FuncGetSize()
	if width <= 0 {
		width = 80
	}

	// The listener owns all in-place drawing: it applies the key to the focused
	// panel (Tab / Shift+Tab switch tabs and never reach the panel), then repaints
	// the whole UI. Enter is special — readline returns from Readline() without
	// invoking the listener — so it is handled in the loop below instead.
	c.SetListener(func(line []rune, pos int, key rune) ([]rune, int, bool) {
		switch key {
		case 0: // initial invocation (nil, 0, 0)
		case readline.CharTab:
			t.focused = (t.focused + 1) % len(t.Panels)
		case readline.MetaShiftTab:
			t.focused = (t.focused - 1 + len(t.Panels)) % len(t.Panels)
		default:
			t.Panels[t.focused].HandleKey(key)
		}
		t.render(sb, width, size)
		// Returning an empty line keeps readline's edit buffer clear so typed
		// runes (j/k/Space/…) never accumulate under our own rendering.
		return nil, 0, true
	})

	for {
		_, rerr := rl.Readline()
		if rerr != nil {
			switch {
			case rerr == readline.ErrInterrupt, rerr.Error() == "Interrupt":
				err = ErrInterrupt
			case rerr == io.EOF:
				err = ErrEOF
			default:
				err = rerr
			}
			break
		}
		// Enter: give the focused panel a chance to consume it (e.g. BrowserPanel
		// descends into a directory). If it does, redraw and keep looping;
		// otherwise the selection is committed.
		if t.Panels[t.focused].HandleKey(KeyEnter) {
			t.render(sb, width, size)
			continue
		}
		break
	}

	sb.Reset()
	sb.Clear()
	sb.Flush()
	rl.Write([]byte(showCursor))
	rl.Close()

	if err != nil {
		return 0, err
	}
	return t.focused, nil
}

// render repaints the tab bar, the focused panel's body, and the help line.
func (t *Tabbed) render(sb *screenbuf.ScreenBuf, width, size int) {
	sb.Reset()
	sb.WriteString(t.tabBar(width))
	// size is the panel's body-row budget; the tab bar and help line sit above
	// and below it (like Select's Size counting list rows, not the whole frame).
	for _, ln := range t.Panels[t.focused].Render(width, size) {
		sb.WriteString(ln)
	}
	// The help line is selector-oriented by default; a panel may tailor it (e.g. a
	// read-only ViewPanel advertises pan/scroll instead of toggle/confirm).
	hint := "↑↓ move · ←→ page · Space toggle · Enter confirm · q/Esc cancel"
	if h, ok := t.Panels[t.focused].(interface{ HelpHint() string }); ok {
		hint = h.HelpHint()
	}
	sb.WriteString(FGFaintStyle("Tab switch · " + hint))
	sb.Flush()
}

// tabSep separates tabs in the bar — a dim, delicate divider.
const tabSep = " · "

// tabBar renders the tab titles left to right: the focused tab is a padded chip
// with a cyan background and dark text (foreground and background swapped), the
// rest dim, separated by a faint divider. Titles are measured with RuneWidth so
// CJK tabs line up, and the bar is truncated to width.
func (t *Tabbed) tabBar(width int) string {
	var b strings.Builder
	used := 0
	for i, p := range t.Panels {
		sep := ""
		if i > 0 {
			sep = tabSep
		}
		title := p.Title()
		// The active tab is a padded chip, so its visible width is the title plus
		// the two padding spaces.
		vis := t.strWidth(title)
		if i == t.focused {
			vis += 2
		}
		if used+t.strWidth(sep)+vis > width && used > 0 {
			break // no room for another tab
		}
		if sep != "" {
			b.WriteString(FGFaintStyle(sep))
		}
		if i == t.focused {
			b.WriteString(Styler(BGCyan, FGBlack)(" " + title + " "))
		} else {
			b.WriteString(FGFaintStyle(title))
		}
		used += t.strWidth(sep) + vis
	}
	return b.String()
}

// strWidth is the display width of s via the injected RuneWidth.
func (t *Tabbed) strWidth(s string) int {
	w := 0
	for _, r := range s {
		w += t.rw(r)
	}
	return w
}

// FGFaintStyle dims a string (help line, inactive tabs, unselected rows).
var FGFaintStyle = Styler(FGFaint)

// rw returns the display width of r via the injected RuneWidth (1 if unset).
func (t *Tabbed) rw(r rune) int {
	if t.RuneWidth != nil {
		return t.RuneWidth(r)
	}
	return 1
}

// ---------------------------------------------------------------------------
// ListPanel — a single- or multi-select list.
// ---------------------------------------------------------------------------

// ListPanel is a scrollable list of string items. With Multi set it becomes a
// checkbox multi-select (Space toggles the highlighted row); otherwise it is a
// single-select whose Selected reports the highlighted row.
type ListPanel struct {
	// TitleText is the tab label.
	TitleText string
	// Items are the rows to display.
	Items []string
	// Multi turns the list into a checkbox multi-select.
	Multi bool
	// RuneWidth returns a rune's display width (CJK = 2); nil means 1 per rune.
	RuneWidth func(rune) int

	cursor  int
	scroll  int
	height  int // last body height Render saw, for page up/down
	checked map[int]bool
}

// NewListPanel builds a ListPanel. multi selects checkbox mode.
func NewListPanel(title string, items []string, multi bool) *ListPanel {
	return &ListPanel{TitleText: title, Items: items, Multi: multi, checked: map[int]bool{}}
}

// Title implements Panel.
func (p *ListPanel) Title() string { return p.TitleText }

// HandleKey implements Panel: ↑↓ (vim jk) navigate, ←→ (vim hl) page, g/G jump
// to top/bottom, Space toggles the highlighted row (multi only). Enter is left
// for the container to commit, so it is never consumed here.
func (p *ListPanel) HandleKey(key rune) (consumed bool) {
	switch {
	case key == KeyEnter:
		return false // container commits
	case key == KeyNext || key == 'j':
		if p.cursor < len(p.Items)-1 {
			p.cursor++
		}
	case key == KeyPrev || key == 'k':
		if p.cursor > 0 {
			p.cursor--
		}
	case key == KeyBackward || key == 'h':
		p.page(-1)
	case key == KeyForward || key == 'l':
		p.page(1)
	case key == 'g':
		p.cursor = 0
	case key == 'G':
		if len(p.Items) > 0 {
			p.cursor = len(p.Items) - 1
		}
	case p.Multi && key == ' ':
		if p.cursor >= 0 && p.cursor < len(p.Items) {
			if p.checked == nil {
				p.checked = map[int]bool{}
			}
			p.checked[p.cursor] = !p.checked[p.cursor]
		}
	default:
		return false
	}
	return true
}

// Render implements Panel: the active row gets ▸ + cyan, others two leading
// spaces; multi mode prefixes each row with a [x]/[ ] checkbox. The visible
// window scrolls to keep the cursor in view.
func (p *ListPanel) Render(width, height int) []string {
	p.height = height
	if len(p.Items) == 0 {
		return []string{FGFaintStyle("  (empty)")}
	}
	p.reScroll(height)
	end := p.scroll + height
	if end > len(p.Items) {
		end = len(p.Items)
	}
	out := make([]string, 0, end-p.scroll)
	for i := p.scroll; i < end; i++ {
		row := p.Items[i]
		if p.Multi {
			box := "[ ] "
			if p.checked[i] {
				box = "[x] "
			}
			row = box + row
		}
		if i == p.cursor {
			out = append(out, "▸ "+Styler(FGCyan)(truncate(row, width-2, p.rw)))
		} else {
			out = append(out, "  "+truncate(row, width-2, p.rw))
		}
	}
	return out
}

// reScroll adjusts the scroll offset so the cursor stays within the window.
func (p *ListPanel) reScroll(height int) {
	if height < 1 {
		height = 1
	}
	if p.cursor < p.scroll {
		p.scroll = p.cursor
	} else if p.cursor >= p.scroll+height {
		p.scroll = p.cursor - height + 1
	}
	if p.scroll < 0 {
		p.scroll = 0
	}
}

// page moves the cursor by dir full windows (dir = -1 up, +1 down), clamped to
// the item bounds; the window height is the last one Render saw.
func (p *ListPanel) page(dir int) {
	step := p.height
	if step < 1 {
		step = 1
	}
	p.cursor += dir * step
	if p.cursor < 0 {
		p.cursor = 0
	}
	if max := len(p.Items) - 1; p.cursor > max {
		p.cursor = max
	}
}

// Selected returns the chosen row indices in ascending order. In multi mode it
// is every checked row; in single mode it is the highlighted row (a 1-element
// slice), or empty when there are no items.
func (p *ListPanel) Selected() []int {
	if !p.Multi {
		if len(p.Items) == 0 {
			return nil
		}
		return []int{p.cursor}
	}
	out := make([]int, 0, len(p.checked))
	for i, on := range p.checked {
		if on {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Cursor returns the highlighted row index.
func (p *ListPanel) Cursor() int { return p.cursor }

// SetCursor pre-positions the highlight on row i, clamped to the item bounds
// (used to open a tab on the currently active value).
func (p *ListPanel) SetCursor(i int) {
	if max := len(p.Items) - 1; i > max {
		i = max
	}
	if i < 0 {
		i = 0
	}
	p.cursor = i
}

func (p *ListPanel) rw(r rune) int {
	if p.RuneWidth != nil {
		return p.RuneWidth(r)
	}
	return 1
}

// ---------------------------------------------------------------------------
// BrowserPanel — a directory navigator that chooses a file.
// ---------------------------------------------------------------------------

// browserItem is one navigable row: "../", a subdirectory, or a file.
type browserItem struct {
	label string
	path  string
	isDir bool
}

// BrowserPanel walks the filesystem starting at Dir. ↑↓ navigate; Enter on a
// directory descends into it (consumed, no submit), Enter on a file records it
// (not consumed, so the container submits). Hidden entries are skipped. This
// reuses the same directory-reading rules as the old pickFile browser.
type BrowserPanel struct {
	// TitleText is the tab label.
	TitleText string
	// Dir is the starting directory (defaults to the working directory).
	Dir string
	// RuneWidth returns a rune's display width (CJK = 2); nil means 1 per rune.
	RuneWidth func(rune) int

	dir     string
	items   []browserItem
	cursor  int
	scroll  int
	rows    int // last entry-row count Render saw, for page up/down
	chosen  string
	err     error
	scanned bool
}

// NewBrowserPanel builds a BrowserPanel rooted at dir (empty → working dir).
func NewBrowserPanel(title, dir string) *BrowserPanel {
	return &BrowserPanel{TitleText: title, Dir: dir}
}

// Title implements Panel.
func (b *BrowserPanel) Title() string { return b.TitleText }

// ensure lazily reads the current directory on first use.
func (b *BrowserPanel) ensure() {
	if b.scanned {
		return
	}
	b.scanned = true
	dir := b.Dir
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else {
			dir, _ = os.UserHomeDir()
		}
	}
	b.setDir(dir)
}

// setDir loads dir's entries into items and resets the cursor.
func (b *BrowserPanel) setDir(dir string) {
	items, err := readDirItems(dir)
	if err != nil {
		b.err = err
		return
	}
	b.dir = dir
	b.items = items
	b.cursor = 0
	b.scroll = 0
	b.err = nil
}

// readDirItems lists a directory as "../" + subdirs + files, skipping hidden
// entries — the shared browser-listing logic (formerly inline in pickFile).
func readDirItems(dir string) ([]browserItem, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var items []browserItem
	if parent := filepath.Dir(dir); parent != dir {
		items = append(items, browserItem{"../", parent, true})
	}
	var dirs, files []browserItem
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // skip hidden entries
		}
		p := filepath.Join(dir, name)
		if e.IsDir() {
			dirs = append(dirs, browserItem{name + "/", p, true})
		} else {
			files = append(files, browserItem{name, p, false})
		}
	}
	items = append(items, dirs...)
	items = append(items, files...)
	return items, nil
}

// HandleKey implements Panel: ↑↓ (vim jk) navigate, ←→ (vim hl) page; Enter on a
// directory descends (consumed) and on a file records the choice (not consumed →
// the container submits).
func (b *BrowserPanel) HandleKey(key rune) (consumed bool) {
	b.ensure()
	switch {
	case key == KeyEnter:
		if b.cursor < 0 || b.cursor >= len(b.items) {
			return false
		}
		it := b.items[b.cursor]
		if it.isDir {
			b.setDir(it.path)
			return true // descended; do not submit
		}
		b.chosen = it.path
		return false // file chosen; let the container submit
	case key == KeyNext || key == 'j':
		if b.cursor < len(b.items)-1 {
			b.cursor++
		}
	case key == KeyPrev || key == 'k':
		if b.cursor > 0 {
			b.cursor--
		}
	case key == KeyBackward || key == 'h':
		b.page(-1)
	case key == KeyForward || key == 'l':
		b.page(1)
	case key == 'g':
		b.cursor = 0
	case key == 'G':
		if len(b.items) > 0 {
			b.cursor = len(b.items) - 1
		}
	default:
		return false
	}
	return true
}

// Render implements Panel: the current directory header (dim) followed by the
// scrollable entry list with ▸ + cyan on the active row.
func (b *BrowserPanel) Render(width, height int) []string {
	b.ensure()
	if b.err != nil {
		return []string{Styler(FGRed)(truncate(b.err.Error(), width, b.rw))}
	}
	out := make([]string, 0, height)
	out = append(out, FGFaintStyle(truncate(b.dir, width, b.rw)))
	rows := height - 1 // one row taken by the dir header
	if rows < 1 {
		rows = 1
	}
	b.rows = rows
	b.reScroll(rows)
	end := b.scroll + rows
	if end > len(b.items) {
		end = len(b.items)
	}
	for i := b.scroll; i < end; i++ {
		it := b.items[i]
		if i == b.cursor {
			out = append(out, "▸ "+Styler(FGCyan)(truncate(it.label, width-2, b.rw)))
		} else {
			out = append(out, "  "+truncate(it.label, width-2, b.rw))
		}
	}
	return out
}

func (b *BrowserPanel) reScroll(height int) {
	if height < 1 {
		height = 1
	}
	if b.cursor < b.scroll {
		b.scroll = b.cursor
	} else if b.cursor >= b.scroll+height {
		b.scroll = b.cursor - height + 1
	}
	if b.scroll < 0 {
		b.scroll = 0
	}
}

// page moves the cursor by dir full windows (dir = -1 up, +1 down), clamped to
// the entry bounds; the window height is the last one Render saw.
func (b *BrowserPanel) page(dir int) {
	step := b.rows
	if step < 1 {
		step = 1
	}
	b.cursor += dir * step
	if b.cursor < 0 {
		b.cursor = 0
	}
	if max := len(b.items) - 1; b.cursor > max {
		b.cursor = max
	}
}

// Chosen returns the absolute path of the picked file, or "" if none was chosen.
func (b *BrowserPanel) Chosen() string { return b.chosen }

func (b *BrowserPanel) rw(r rune) int {
	if b.RuneWidth != nil {
		return b.RuneWidth(r)
	}
	return 1
}

// truncate clips s to at most width display columns (per rw), appending "…" when
// it had to cut. width<=0 yields an empty string.
func truncate(s string, width int, rw func(rune) int) string {
	if width <= 0 {
		return ""
	}
	total := 0
	for _, r := range s {
		total += rw(r)
	}
	if total <= width {
		return s
	}
	// Reserve one column for the ellipsis.
	limit := width - 1
	if limit < 0 {
		limit = 0
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := rw(r)
		if used+w > limit {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString("…")
	return b.String()
}

// ---------------------------------------------------------------------------
// ViewPanel — a read-only, scrollable text view.
// ---------------------------------------------------------------------------

// ViewPanel displays read-only lines that may exceed the window. By default it
// clips each line and pans horizontally with h/l (like Viewer); set Wrap to
// soft-wrap to the width instead (ANSI-aware, reusing Viewer's wrapLine, pan
// disabled). It scrolls with ↑↓, pages vertically with ←→ / Space / b, and jumps
// with g/G; it never selects anything, so Enter is left for the container to close
// the view. CJK width is approximated as 1, matching Viewer. Used by the merged
// /tools command (a "Tools" tab and an "MCP" tab).
type ViewPanel struct {
	// TitleText is the tab label.
	TitleText string
	// Lines are the content lines (may carry ANSI); each is one logical line with
	// no \r or \n.
	Lines []string
	// Wrap soft-wraps each logical line to the width instead of clipping it;
	// horizontal panning (h/l) is then disabled.
	Wrap bool

	voff     int // vertical scroll offset, in display rows
	hoff     int // horizontal pan offset, in columns (Wrap == false only)
	height   int // last body height Render saw, for paging
	maxLineW int // widest visible line, for the hoff clamp (Wrap == false)

	wrapped   []string // Lines soft-wrapped to wrapWidth (Wrap == true)
	wrapWidth int      // width the wrap cache was built for
}

// NewViewPanel builds a read-only ViewPanel over lines (clip + h/l pan by
// default; set .Wrap for soft-wrapping).
func NewViewPanel(title string, lines []string) *ViewPanel {
	return &ViewPanel{TitleText: title, Lines: lines}
}

// Title implements Panel.
func (p *ViewPanel) Title() string { return p.TitleText }

// HelpHint tailors the container's help line to a read-only viewer (the generic
// selector help — Space toggle / Enter confirm — doesn't apply here).
func (p *ViewPanel) HelpHint() string {
	if p.Wrap {
		return "↑↓ scroll · ←→ page · g/G top/bottom · q/Esc close"
	}
	return "↑↓ scroll · ←→ page · h/l pan · g/G top/bottom · q/Esc close"
}

// HandleKey implements Panel: ↑↓ (vim jk) scroll a line, ←→ / Space / b page
// vertically, h/l pan horizontally (unless Wrap), g/G jump to top/bottom. Enter is
// not consumed, so the container closes the view.
func (p *ViewPanel) HandleKey(key rune) (consumed bool) {
	switch {
	case key == KeyEnter:
		return false // container closes the view
	case key == KeyPrev || key == 'k':
		p.voff--
	case key == KeyNext || key == 'j':
		p.voff++
	case key == KeyForward || key == ' ':
		p.voff += p.pageStep()
	case key == KeyBackward || key == 'b':
		p.voff -= p.pageStep()
	case !p.Wrap && key == 'h':
		p.hoff-- // pan left
	case !p.Wrap && key == 'l':
		p.hoff++ // pan right
	case key == 'g':
		p.voff, p.hoff = 0, 0
	case key == 'G':
		p.voff = len(p.rows()) // Render clamps to the last page
	default:
		return false
	}
	return true
}

// rows returns the current display rows: the wrapped lines when Wrap is set,
// otherwise the raw logical lines (clipped/panned at Render time).
func (p *ViewPanel) rows() []string {
	if p.Wrap {
		return p.wrapped
	}
	return p.Lines
}

// Render implements Panel: the visible slice of the content, scrolled by voff and
// (when not wrapping) panned by hoff. The wrap is cached and rebuilt only when the
// width changes.
func (p *ViewPanel) Render(width, height int) []string {
	p.height = height

	var lines []string
	if p.Wrap {
		if p.wrapped == nil || p.wrapWidth != width {
			var w []string
			for _, ln := range p.Lines {
				w = append(w, wrapLine(ln, width)...)
			}
			p.wrapped = w
			p.wrapWidth = width
		}
		lines = p.wrapped
	} else {
		lines = p.Lines
		p.maxLineW = 0
		for _, ln := range lines {
			if w := visibleWidth(ln); w > p.maxLineW {
				p.maxLineW = w
			}
		}
	}

	p.clampV(len(lines))
	p.clampH()

	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		idx := p.voff + i
		if idx >= len(lines) {
			break
		}
		if p.Wrap {
			out = append(out, lines[idx])
		} else {
			out = append(out, clipLine(lines[idx], p.hoff, width))
		}
	}
	return out
}

// clampV keeps voff within [0, n-height].
func (p *ViewPanel) clampV(n int) {
	max := n - p.height
	if max < 0 {
		max = 0
	}
	if p.voff > max {
		p.voff = max
	}
	if p.voff < 0 {
		p.voff = 0
	}
}

// clampH keeps hoff within [0, maxLineW].
func (p *ViewPanel) clampH() {
	if p.hoff > p.maxLineW {
		p.hoff = p.maxLineW
	}
	if p.hoff < 0 {
		p.hoff = 0
	}
}

func (p *ViewPanel) pageStep() int {
	if p.height > 1 {
		return p.height
	}
	return 1
}
