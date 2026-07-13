package ui

import (
	"context"
	"fmt"
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

type previewState struct {
	label string
	tail  []string // last previewWindow raw source lines
}

type selectState struct {
	spec  SelectSpec
	idx   int
	reply chan SelectResult
}

type viewState struct {
	spec   ViewSpec
	offset int
	height int
	reply  chan struct{}
}

// model is the tea.Model behind the facade. All state mutation happens inside
// Update (single-threaded); the facade reaches in only via messages.
type model struct {
	ta     textarea.Model
	width  int
	height int
	widthO *atomic.Int64 // shared with UI for facade-side wrapping

	status StatusData
	glyph  int // advances per insert: the view change that defeats viewEquals
	title  string

	queue  []string
	waiter chan inputResult // pending ReadInput; nil when logic is processing

	busy    *busyState
	preview *previewState
	sel     *selectState
	viewer  *viewState

	cancels []context.CancelFunc // interrupt scope stack (bottom = turn)

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

	case printedMsg:
		m.glyph++ // view changes → flush runs → real cursor restored post-insert
		return m, nil

	case statusMsg:
		m.status = StatusData(msg)
		return m, nil

	case titleMsg:
		m.title = string(msg)
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
			msg.reply <- inputResult{in: Input{Display: item, Text: item}}
			return m, nil
		}
		m.waiter = msg.reply
		return m, nil

	case readCancelMsg:
		if m.waiter == msg.reply {
			m.waiter = nil
		}
		return m, nil

	case selectOpenMsg:
		m.sel = &selectState{spec: msg.spec, idx: msg.spec.Cursor, reply: msg.reply}
		if m.sel.idx < 0 || m.sel.idx >= len(msg.spec.Items) {
			m.sel.idx = 0
		}
		return m, nil

	case viewOpenMsg:
		h := msg.spec.Height
		if h <= 0 || h > 15 {
			h = 15
		}
		if n := len(msg.spec.Lines); n < h {
			h = n
		}
		m.viewer = &viewState{spec: msg.spec, height: h, reply: msg.reply}
		return m, nil

	case surfaceCancelMsg:
		m.closeSelect(SelectResult{Cancelled: true})
		m.closeViewer()
		return m, nil

	case previewOpenMsg:
		m.preview = &previewState{label: msg.label}
		return m, m.ensureSpin()

	case previewLineMsg:
		if m.preview != nil {
			m.preview.tail = append(m.preview.tail, msg.line)
			if len(m.preview.tail) > previewWindow {
				m.preview.tail = m.preview.tail[len(m.preview.tail)-previewWindow:]
			}
		}
		return m, nil

	case previewCloseMsg:
		m.preview = nil
		return m, nil

	case spinTickMsg:
		if m.busy == nil && m.preview == nil {
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

	// Surfaces own the keys while open (rendered below the composer).
	if m.sel != nil {
		switch k.Code {
		case tea.KeyUp:
			if m.sel.idx > 0 {
				m.sel.idx--
			}
		case tea.KeyDown:
			if m.sel.idx < len(m.sel.spec.Items)-1 {
				m.sel.idx++
			}
		case tea.KeyEnter:
			m.closeSelect(SelectResult{Index: m.sel.idx})
		case tea.KeyEscape:
			m.closeSelect(SelectResult{Cancelled: true})
		default:
			if k.Text == "q" || (k.Mod == tea.ModCtrl && k.Code == 'c') {
				m.closeSelect(SelectResult{Cancelled: true})
			}
		}
		return m, nil
	}
	if m.viewer != nil {
		v := m.viewer
		maxOff := len(v.spec.Lines) - v.height
		if maxOff < 0 {
			maxOff = 0
		}
		switch k.Code {
		case tea.KeyUp:
			if v.offset > 0 {
				v.offset--
			}
		case tea.KeyDown:
			if v.offset < maxOff {
				v.offset++
			}
		case tea.KeyEscape, tea.KeyEnter:
			m.closeViewer()
		default:
			if k.Text == "q" || (k.Mod == tea.ModCtrl && k.Code == 'c') {
				m.closeViewer()
			}
		}
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

	if k.Code == tea.KeyEnter {
		text := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.ta.SetHeight(1) // collapse sheds bottom rows — self-healing
		if text == "" {
			return m, nil
		}
		in := Input{Display: text, Text: text}
		if m.waiter != nil {
			m.waiter <- inputResult{in: in}
			m.waiter = nil
		} else {
			m.queue = append(m.queue, text) // type-ahead: queue until ReadInput
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.resizeComposer()
	return m, cmd
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

func (m *model) closeSelect(r SelectResult) {
	if m.sel == nil {
		return
	}
	m.sel.reply <- r
	m.sel = nil
}

func (m *model) closeViewer() {
	if m.viewer == nil {
		return
	}
	m.viewer.reply <- struct{}{}
	m.viewer = nil
}

func (m *model) View() tea.View {
	var b strings.Builder
	rowsAbove := 0 // frame rows above the textarea, to offset the real cursor

	// Busy spinner and the block preview render on the CONTENT side (above the
	// separator): they are content-in-progress, not interaction (unlike the
	// surfaces below the composer). The close-shrink bounce this position
	// implies is minimized by the adapter deferring the preview close until
	// the rendered block arrives (adjacent messages — at most one frame).
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
	if m.preview != nil {
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + sgrReset +
			faint + " " + m.preview.label + sgrReset + "\n")
		for _, line := range m.preview.tail {
			b.WriteString(faint + "  " + ansi.Truncate(line, maxInt(4, m.width-4), "…") + sgrReset + "\n")
		}
		rowsAbove += 1 + len(m.preview.tail)
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

	// Interaction surfaces BELOW the composer: the composer never moves when
	// they close, and their vacated rows die at the screen bottom where output
	// self-heals them (see docs/design/ui-architecture.md).
	if m.sel != nil {
		b.WriteString("\n" + faint + "── " + m.sel.spec.Title + "(↑↓ Enter · ESC 取消)──" + sgrReset)
		for i, it := range m.sel.spec.Items {
			if i == m.sel.idx {
				b.WriteString("\n" + cyan + "▸ " + it + sgrReset)
			} else {
				b.WriteString("\n  " + it)
			}
		}
	}
	if m.viewer != nil {
		v := m.viewer
		b.WriteString("\n" + faint + "── " + v.spec.Title + "(↑↓ 滚动 · q 关闭)──" + sgrReset)
		end := v.offset + v.height
		if end > len(v.spec.Lines) {
			end = len(v.spec.Lines)
		}
		for _, line := range v.spec.Lines[v.offset:end] {
			b.WriteString("\n" + ansi.Truncate(line, maxInt(4, m.width), "…"))
		}
	}

	view := tea.NewView(b.String())
	view.WindowTitle = m.title
	if m.sel == nil && m.viewer == nil {
		if c := m.ta.Cursor(); c != nil {
			c.Y += rowsAbove
			view.Cursor = c
		}
	}
	return view
}

// statusLine renders " model · ctx used/window (pct) ⠋" — the trailing glyph
// advances on every insert, which is load-bearing: it keeps the view changing
// per committed line so the renderer's flush (and its cursor MoveTo) runs.
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
	glyph := spinnerFrames[m.glyph%len(spinnerFrames)]

	plain := "  " + model + " · " + ctx + " " + glyph
	if textwidth.StringWidth(plain) > m.width {
		return faint + ansi.Truncate(plain, maxInt(4, m.width), "…") + sgrReset
	}
	return "  " + cyan + faint + model + sgrReset + faint + " · " + sgrReset +
		green + faint + ctx + sgrReset + " " + faint + glyph + sgrReset
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
