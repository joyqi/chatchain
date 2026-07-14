package ui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"chatchain/internal/textwidth"
)

// Raw SGR styles — the frame is deliberately dependency-light.
const (
	faint    = "\x1b[2m"
	cyan     = "\x1b[36m"
	green    = "\x1b[32m"
	revOn    = "\x1b[7m"
	sgrReset = "\x1b[0m"
)

// spinnerFrames drive the busy/preview headers and the per-insert activity
// glyph (braille dots, single column — width-safe).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// maxComposerRows caps the composer's dynamic growth; longer input scrolls
// inside the textarea.
const maxComposerRows = 5

// queueShownMax caps visible queued lines; beyond it a "+N more" row.
const queueShownMax = 3

// previewWindow is the rolling raw-source window of a block preview.
const previewWindow = 3

type busyState struct {
	label string
	since time.Time
}

// model is the tea.Model behind the facade. All state mutation happens inside
// Update (single-threaded); the facade reaches in only via messages.
type model struct {
	ta     textarea.Model
	width  int
	height int
	widthO *atomic.Int64 // shared with UI for facade-side wrapping

	status StatusData
	title  string

	queue  []string
	waiter chan inputResult // pending ReadInput; nil when logic is processing

	busy    *busyState
	region  regionMsg // staging window snapshot (tail + preview), see region.go
	surf    *surfaceState
	surfGen int

	cancels []context.CancelFunc // interrupt scope stack (bottom = turn)

	// Input history (session-local, like the readline path): submitted and
	// queued inputs, ↑/↓ navigable while the composer holds a single row.
	history   []string
	histIdx   int    // == len(history) when not navigating
	histDraft string // draft saved when navigation starts

	// Slash completion: commands installed via SetSlashCommands; Tab cycles
	// the matches of the prefix captured at the first Tab press.
	commands []string
	sugBase  string // "" = not cycling
	sugIdx   int

	// Paste store: multi-line pastes collapse to "[#N …]" tags in the
	// composer; Input.Text expands them on submit, Display keeps the tag.
	pastes []string

	spin        int
	spinTicking bool
}

func newModel(widthO *atomic.Int64) *model {
	ta := textarea.New()
	ta.SetVirtualCursor(false) // REAL terminal cursor: IME preedit anchors here
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetPromptFunc(2, func(pi textarea.PromptInfo) string {
		if pi.LineNumber == 0 {
			return cyan + "❯ " + sgrReset
		}
		return "  " // soft-wrapped continuation rows align under the input
	})
	ta.SetWidth(80)
	ta.Focus()
	return &model{ta: ta, width: 80, widthO: widthO}
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width)
		if m.widthO != nil {
			m.widthO.Store(int64(msg.Width))
		}
		return m, nil

	case regionMsg:
		// Every snapshot changes the rendered staging window, which is the
		// view change that defeats the renderer's viewEquals early-return and
		// restores the real cursor after each insert (no extra glyph needed).
		m.region = msg
		if msg.label != "" {
			return m, m.ensureSpin()
		}
		return m, nil

	case statusMsg:
		m.status = StatusData(msg)
		return m, nil

	case titleMsg:
		m.title = string(msg)
		return m, nil

	case setCommandsMsg:
		m.commands = msg
		return m, nil

	case tea.PasteMsg:
		if m.surf != nil {
			return m, nil
		}
		content := strings.ReplaceAll(msg.Content, "\r\n", "\n")
		content = strings.ReplaceAll(content, "\r", "\n")
		content = strings.TrimRight(content, "\n")
		if strings.Contains(content, "\n") {
			m.pastes = append(m.pastes, content)
			m.ta.InsertString(pasteTag(len(m.pastes), content))
		} else {
			m.ta.InsertString(content)
		}
		m.resizeComposer()
		return m, nil

	case busyOnMsg:
		m.busy = &busyState{label: msg.label, since: time.Now()}
		return m, m.ensureSpin()

	case busyOffMsg:
		m.busy = nil
		return m, nil

	case scopePushMsg:
		m.cancels = append(m.cancels, msg.cancel)
		return m, nil

	case scopePopMsg:
		if n := len(m.cancels); n > 0 {
			m.cancels = m.cancels[:n-1]
		}
		return m, nil

	case readReqMsg:
		if len(m.queue) > 0 {
			item := m.queue[0]
			m.queue = m.queue[1:]
			msg.reply <- inputResult{in: m.makeInput(item)}
			return m, nil
		}
		m.waiter = msg.reply
		return m, nil

	case readCancelMsg:
		if m.waiter == msg.reply {
			m.waiter = nil
		}
		return m, nil

	case tabbedOpenMsg:
		m.surfGen++
		m.surf = newSurface(msg.spec, msg.reply, m.surfGen)
		if msg.spec.RefreshEvery > 0 {
			gen := m.surfGen
			return m, tea.Tick(time.Duration(msg.spec.RefreshEvery)*time.Millisecond,
				func(time.Time) tea.Msg { return surfTickMsg{gen: gen} })
		}
		return m, nil

	case surfTickMsg:
		if m.surf == nil || m.surf.gen != msg.gen {
			return m, nil // stale tick from a closed surface
		}
		for i, p := range m.surf.spec.Panels {
			if p.Refresh != nil {
				st := &m.surf.ps[i]
				st.items = p.Refresh()
				if st.cursor >= len(st.items) {
					st.cursor = maxInt(0, len(st.items)-1)
				}
			}
		}
		gen := msg.gen
		return m, tea.Tick(time.Duration(m.surf.spec.RefreshEvery)*time.Millisecond,
			func(time.Time) tea.Msg { return surfTickMsg{gen: gen} })

	case surfaceCancelMsg:
		m.closeSurface(TabbedResult{Cancelled: true})
		return m, nil

	case spinTickMsg:
		if m.busy == nil && m.region.label == "" {
			m.spinTicking = false
			return m, nil
		}
		m.spin++
		return m, tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })

	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// ensureSpin starts the spinner tick chain if an animated header is on screen.
func (m *model) ensureSpin() tea.Cmd {
	if m.spinTicking {
		return nil
	}
	m.spinTicking = true
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
}

func (m *model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.Key()

	// The surface owns the keys while open (rendered below the composer).
	if m.surf != nil {
		m.surfaceKey(k)
		return m, nil
	}

	// Interrupts. With scopes active, ESC cancels the innermost (tool) scope
	// and Ctrl+C the turn; the type-ahead queue is restored into the composer
	// draft atomically. At idle, Ctrl+C/Ctrl+D end the session (ErrInterrupted
	// to the waiting ReadInput).
	if k.Mod == tea.ModCtrl && (k.Code == 'c' || k.Code == 'd') {
		if len(m.cancels) > 0 {
			m.fireCancel(0) // the turn scope
			return m, nil
		}
		if m.waiter != nil {
			m.waiter <- inputResult{err: ErrInterrupted}
			m.waiter = nil
		}
		return m, nil
	}
	if k.Code == tea.KeyEscape && len(m.cancels) > 0 {
		m.fireCancel(len(m.cancels) - 1) // the innermost scope
		return m, nil
	}

	// Tab cycles slash-command completions for the prefix captured at the
	// first press; any other key ends the cycle.
	if k.Code == tea.KeyTab {
		base := m.sugBase
		if base == "" {
			base = m.ta.Value()
		}
		if ms := matchCommands(m.commands, base); len(ms) > 0 {
			if m.sugBase == "" {
				m.sugBase = base
				m.sugIdx = -1
			}
			m.sugIdx = (m.sugIdx + 1) % len(ms)
			m.setDraft(ms[m.sugIdx])
		}
		return m, nil
	}
	m.sugBase = ""

	// ↑/↓ walk the input history while the composer holds a single row
	// (multi-row drafts keep the arrows for cursor movement).
	if k.Code == tea.KeyUp && m.historyNavigable() {
		if m.histIdx == len(m.history) {
			m.histDraft = m.ta.Value()
		}
		if m.histIdx > 0 {
			m.histIdx--
			m.setDraft(m.history[m.histIdx])
		}
		return m, nil
	}
	if k.Code == tea.KeyDown && m.historyNavigable() && m.histIdx < len(m.history) {
		m.histIdx++
		if m.histIdx == len(m.history) {
			m.setDraft(m.histDraft)
		} else {
			m.setDraft(m.history[m.histIdx])
		}
		return m, nil
	}

	if k.Code == tea.KeyEnter {
		text := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.ta.SetHeight(1) // collapse sheds bottom rows — self-healing
		if text == "" {
			return m, nil
		}
		m.pushHistory(text)
		if m.waiter != nil {
			m.waiter <- inputResult{in: m.makeInput(text)}
			m.waiter = nil
		} else {
			m.queue = append(m.queue, text) // type-ahead: queue until ReadInput
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.resizeComposer()
	m.histIdx = len(m.history) // any edit leaves history navigation
	return m, cmd
}

// pushHistory records a submitted/queued input and resets navigation.
func (m *model) pushHistory(text string) {
	if n := len(m.history); n > 0 && m.history[n-1] == text {
		m.histIdx = len(m.history)
		return // collapse immediate duplicates, like readline
	}
	m.history = append(m.history, text)
	m.histIdx = len(m.history)
}

// makeInput builds the Display/Text pair: Display keeps paste tags, Text
// expands them to the stored paste contents.
func (m *model) makeInput(text string) Input {
	return Input{Display: text, Text: expandPasteTags(text, m.pastes)}
}

// historyNavigable: single-row composer content only.
func (m *model) historyNavigable() bool {
	return m.ta.LineCount() <= 1 && m.ta.LineInfo().Height <= 1
}

// setDraft replaces the composer content (cursor at end, height adjusted).
func (m *model) setDraft(s string) {
	m.ta.SetValue(s)
	m.ta.MoveToEnd()
	m.resizeComposer()
}

// fireCancel invokes the cancel at stack index i (callers pass 0 for the turn,
// top for the innermost) and restores any queued submits into the composer
// draft — interrupt means "the situation changed", so nothing auto-sends.
func (m *model) fireCancel(i int) {
	cancel := m.cancels[i]
	m.cancels = m.cancels[:i]
	if cancel != nil {
		cancel()
	}
	if len(m.queue) > 0 {
		draft := strings.Join(m.queue, "\n")
		if cur := m.ta.Value(); strings.TrimSpace(cur) != "" {
			draft += "\n" + cur
		}
		m.queue = nil
		m.ta.SetValue(draft)
		rows := m.ta.LineCount()
		if rows > maxComposerRows {
			rows = maxComposerRows
		}
		m.ta.SetHeight(rows)
		m.ta.MoveToEnd()
	}
}

// resizeComposer grows/shrinks the textarea with its soft-wrapped content
// (single logical line; Enter submits). Multi-logical-line drafts (queue
// restore) keep their explicit height — LineInfo covers only the current line.
func (m *model) resizeComposer() {
	if m.ta.LineCount() > 1 {
		return
	}
	contentRows := m.ta.LineInfo().Height
	rows := contentRows
	if rows < 1 {
		rows = 1
	}
	if rows > maxComposerRows {
		rows = maxComposerRows
	}
	if old := m.ta.Height(); rows != old {
		m.ta.SetHeight(rows) // shrink sheds bottom rows — self-healing
		if rows > old && contentRows <= maxComposerRows && m.ta.ScrollYOffset() > 0 {
			// The viewport never scrolls back on growth (repositionView only
			// keeps the cursor visible): snap to the top, keep the column.
			col := m.ta.Column()
			m.ta.MoveToBegin()
			m.ta.SetCursorColumn(col)
		}
	}
}

func (m *model) closeSurface(r TabbedResult) {
	if m.surf == nil {
		return
	}
	m.surf.reply <- r
	m.surf = nil
}

// surfaceKey routes a keypress into the open surface (v1 panel semantics).
func (m *model) surfaceKey(k tea.Key) {
	s := m.surf
	p := s.spec.Panels[s.focus]
	st := &s.ps[s.focus]

	if k.Mod == tea.ModCtrl && k.Code == 'c' {
		m.closeSurface(TabbedResult{Cancelled: true})
		return
	}
	switch k.Code {
	case tea.KeyTab:
		s.focus = (s.focus + 1) % len(s.spec.Panels)
		return
	case tea.KeyEscape:
		m.closeSurface(TabbedResult{Cancelled: true})
		return
	case tea.KeyEnter:
		if p.Kind == PanelBrowser && st.cursor >= 0 && st.cursor < len(st.entries) {
			e := st.entries[st.cursor]
			if e.isDir {
				st.setDir(e.path) // descend; do not submit
				return
			}
			st.chosen = e.path // file chosen; fall through to commit
		}
		m.closeSurface(s.result())
		return
	case tea.KeyUp:
		m.surfaceNav(p, st, -1)
		return
	case tea.KeyDown:
		m.surfaceNav(p, st, 1)
		return
	case tea.KeyLeft:
		if p.Kind == PanelSlider {
			sliderStep(st, p, -1)
		}
		return
	case tea.KeyRight:
		if p.Kind == PanelSlider {
			sliderStep(st, p, 1)
		}
		return
	case tea.KeySpace:
		if p.Kind == PanelMulti {
			st.checked[st.cursor] = !st.checked[st.cursor]
		}
		return
	}
	switch k.Text {
	case "c":
		if p.Kind == PanelView {
			st.copied = copyToClipboard(stripSGRText(strings.Join(st.items, "\n"))) == nil
			return
		}
		m.closeSurface(TabbedResult{Cancelled: true}) // any other key: fallthrough semantics below
		return
	case "q":
		m.closeSurface(TabbedResult{Cancelled: true})
	case " ":
		if p.Kind == PanelMulti {
			st.checked[st.cursor] = !st.checked[st.cursor]
		}
	case "j":
		m.surfaceNav(p, st, 1)
	case "k":
		m.surfaceNav(p, st, -1)
	case "h":
		if p.Kind == PanelSlider {
			sliderStep(st, p, -1)
		}
	case "l":
		if p.Kind == PanelSlider {
			sliderStep(st, p, 1)
		}
	case "g":
		if p.Kind == PanelSlider {
			st.value = nil
		} else {
			st.cursor, st.offset = 0, 0
		}
	case "G":
		switch p.Kind {
		case PanelSlider:
			v := p.Max
			st.value = &v
		case PanelBrowser:
			st.cursor = maxInt(0, len(st.entries)-1)
		default:
			st.cursor = maxInt(0, len(st.items)-1)
		}
	}
}

// surfaceNav moves the cursor / scroll of the focused panel.
func (m *model) surfaceNav(p Panel, st *panelState, dir int) {
	switch p.Kind {
	case PanelList, PanelMulti:
		st.cursor = clampInt(st.cursor+dir, 0, maxInt(0, len(st.items)-1))
	case PanelBrowser:
		st.cursor = clampInt(st.cursor+dir, 0, maxInt(0, len(st.entries)-1))
	case PanelView:
		st.offset = maxInt(0, st.offset+dir) // upper bound clamped at render
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (m *model) View() tea.View {
	var b strings.Builder
	rowsAbove := 0 // frame rows above the textarea, to offset the real cursor

	// The staging window (region.go): the last tailKeep output lines live here,
	// directly above the separator, and block previews morph into their
	// rendered blocks IN PLACE — the window's height never shrinks, so the
	// composer never pops. Rendered as-is (committed content), preview rows dim.
	for _, line := range m.region.tail {
		b.WriteString(line + "\n")
		rowsAbove++
	}
	if m.region.label != "" {
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + sgrReset +
			faint + " " + m.region.label + sgrReset + "\n")
		for _, line := range m.region.ptail {
			b.WriteString(faint + "  " + ansi.Truncate(line, maxInt(4, m.width-4), "…") + sgrReset + "\n")
		}
		rowsAbove += 1 + len(m.region.ptail)
	}

	// Busy spinner (content side; transient single row).
	if m.busy != nil {
		elapsed := int(time.Since(m.busy.since).Seconds())
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + sgrReset +
			faint + " " + m.busy.label)
		if elapsed >= 2 {
			fmt.Fprintf(&b, "  %ds", elapsed)
		}
		if len(m.cancels) > 0 {
			b.WriteString(" (ESC to cancel)")
		}
		b.WriteString(sgrReset + "\n")
		rowsAbove++
	}

	// Type-ahead queue: dim "»" lines above the separator (content side).
	if n := len(m.queue); n > 0 {
		shown := n
		if shown > queueShownMax {
			shown = queueShownMax
		}
		for i := 0; i < shown; i++ {
			item := ansi.Truncate(m.queue[i], maxInt(4, m.width-4), "…")
			if strings.HasPrefix(item, "/") {
				item = green + item + sgrReset + faint
			}
			b.WriteString(faint + "» " + item + sgrReset + "\n")
		}
		if n > shown {
			fmt.Fprintf(&b, "%s  … +%d more%s\n", faint, n-shown, sgrReset)
		}
		rowsAbove += shown
		if n > shown {
			rowsAbove++
		}
	}

	// Separator + status.
	w := m.width
	if w < 1 {
		w = 80
	}
	b.WriteString(faint + strings.Repeat("─", w) + sgrReset + "\n")
	b.WriteString(m.statusLine())
	b.WriteString("\n")
	rowsAbove += 2

	b.WriteString(m.ta.View())

	// Slash suggestions: a dim row of matching commands under the composer
	// while a "/" prefix is being typed (Tab cycles them).
	if m.surf == nil {
		base := m.sugBase
		if base == "" {
			base = m.ta.Value()
		}
		if ms := matchCommands(m.commands, base); len(ms) > 0 {
			var row strings.Builder
			for i, c := range ms {
				if i > 0 {
					row.WriteString("  ")
				}
				if m.sugBase != "" && i == m.sugIdx {
					row.WriteString(cyan + c + sgrReset)
				} else {
					row.WriteString(faint + c + sgrReset)
				}
			}
			b.WriteString("\n" + ansi.Truncate(row.String(), maxInt(4, m.width), "…"))
		}
	}

	// Interaction surfaces BELOW the composer: the composer never moves when
	// they close, and their vacated rows die at the screen bottom where output
	// self-heals them (see docs/design/ui-architecture.md).
	if m.surf != nil {
		m.renderSurface(&b)
	}

	view := tea.NewView(b.String())
	view.WindowTitle = m.title
	if m.surf == nil {
		if c := m.ta.Cursor(); c != nil {
			c.Y += rowsAbove
			view.Cursor = c
		}
	}
	return view
}

// statusLine renders " model · ctx used/window (pct)".
func (m *model) statusLine() string {
	model := m.status.Model
	if model == "" {
		model = "—"
	}
	approx := ""
	if m.status.Estimated {
		approx = "≈"
	}
	ctx := fmt.Sprintf("ctx %s%s / %s", approx, formatTokens(m.status.CtxUsed), formatTokens(m.status.CtxWindow))
	if m.status.CtxWindow > 0 {
		ctx += fmt.Sprintf(" (%d%%)", m.status.CtxUsed*100/m.status.CtxWindow)
	}

	plain := "  " + model + " · " + ctx
	if textwidth.StringWidth(plain) > m.width {
		return faint + ansi.Truncate(plain, maxInt(4, m.width), "…") + sgrReset
	}
	return "  " + cyan + faint + model + sgrReset + faint + " · " + sgrReset +
		green + faint + ctx + sgrReset
}

// formatTokens renders a token count compactly: 128000 → "128k".
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "m"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

func trimZero(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// matchCommands returns the commands matching a "/" prefix being typed (no
// arguments yet); nil when the line isn't a bare command prefix.
func matchCommands(commands []string, line string) []string {
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \n") {
		return nil
	}
	var ms []string
	for _, c := range commands {
		if strings.HasPrefix(c, line) {
			ms = append(ms, c)
		}
	}
	return ms
}

// pasteTag renders the composer stand-in for a stored multi-line paste.
func pasteTag(id int, content string) string {
	lines := strings.Count(content, "\n") + 1
	first := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	r := []rune(first)
	if len(r) > 20 {
		first = string(r[:20])
	}
	return fmt.Sprintf("[#%d %s… %d lines]", id, first, lines)
}

// pasteTagRe matches the composer paste tags for expansion on submit.
var pasteTagRe = regexp.MustCompile(`\[#(\d+)[^\]]*\]`)

// expandPasteTags replaces every stored-paste tag with its full content.
func expandPasteTags(text string, pastes []string) string {
	return pasteTagRe.ReplaceAllStringFunc(text, func(tag string) string {
		mm := pasteTagRe.FindStringSubmatch(tag)
		idx, err := strconv.Atoi(mm[1])
		if err != nil || idx < 1 || idx > len(pastes) {
			return tag
		}
		return pastes[idx-1]
	})
}
