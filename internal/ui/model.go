package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"chatchain/internal/textwidth"
	"chatchain/internal/timefmt"
	"chatchain/internal/tokfmt"
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
	label  string
	detail string // live sub-state ("1.2/5.0 MB", "4.2 KB"); updates keep the clock
	since  time.Time
}

// model is the tea.Model behind the facade. All state mutation happens inside
// Update (single-threaded); the facade reaches in only via messages.
type model struct {
	ta      textarea.Model
	width   int
	height  int
	widthO  *atomic.Int64 // shared with UI for facade-side wrapping
	heightO *atomic.Int64 // shared with UI: region chunks scrollback inserts below screen height

	status StatusData
	title  string

	progress  ProgressState // terminal-native progress indicator (View emits)
	focused   bool          // terminal focus (ReportFocus); gates Notify
	notifyOut io.Writer     // bell/notification sink (stdout; tests inject)

	flushTail func() // region.flushTail, run via cmd on width changes

	queue  []string
	waiter chan inputResult // pending ReadInput; nil when logic is processing

	busy    *busyState
	region  regionMsg // staging window snapshot (tail + preview), see region.go
	surf    *surfaceState
	surfGen int
	surfCur *tea.Cursor // real-cursor target inside the surface (input fields)

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
	// composer and stay collapsed there (including on ↑ recall — a blob is
	// unusable to edit); submit expands them, in full for Text and bounded
	// for the transcript echo.
	pastes []string

	spin        int
	spinTicking bool

	// oneShot: the model renders ONLY its surface (no composer chrome) and
	// quits when the surface closes — the RunSurface pre-REPL mode.
	oneShot bool
}

func newModel(widthO, heightO *atomic.Int64) *model {
	ta := textarea.New()
	ta.SetVirtualCursor(false) // REAL terminal cursor: IME preedit anchors here
	ta.ShowLineNumbers = false
	// Neutral styles: the default focused CursorLine carries a background
	// tint — the composer should render on the terminal's own background.
	styles := textarea.DefaultDarkStyles()
	styles.Focused.CursorLine = styles.Focused.Text
	styles.Blurred.CursorLine = styles.Blurred.Text
	ta.SetStyles(styles)
	ta.SetHeight(1)
	ta.SetPromptFunc(2, func(pi textarea.PromptInfo) string {
		if pi.LineNumber == 0 {
			return cyan + "❯ " + sgrReset
		}
		return "  " // soft-wrapped continuation rows align under the input
	})
	ta.SetWidth(80)
	ta.Focus()
	return &model{ta: ta, width: 80, widthO: widthO, heightO: heightO,
		focused: true, notifyOut: os.Stdout}
}

// progressBarState maps the facade state onto the OSC 9;4 wire states. Input
// and Error carry a FULL bar (Value 100): warning and error are states of the
// whole turn, and a 0% sliver would be invisible in terminals that render the
// percentage (Windows Terminal) — Indeterminate ignores the value.
func progressBarState(s ProgressState) tea.ProgressBarState {
	switch s {
	case ProgressBusy:
		return tea.ProgressBarIndeterminate
	case ProgressInput:
		return tea.ProgressBarWarning
	case ProgressError:
		return tea.ProgressBarError
	}
	return tea.ProgressBarNone
}

// notifyCmd carries an attention ping to the terminal — only while it is
// unfocused: whoever is already watching needs no bell. BOTH standard
// channels ride one write: the OSC 9 desktop notification (its BEL merely
// terminates the sequence) followed by a bare BEL that actually rings —
// hosts listen to one or the other (cmux surfaces OSC 9, plain terminals
// ring), and each terminal's own settings decide presentation. The write
// happens on the cmd goroutine, outside the renderer: both sequences are
// cursor-neutral, and the text is pre-sanitized by the facade.
func (m *model) notifyCmd(text string) tea.Cmd {
	if m.focused {
		return nil
	}
	if strings.HasPrefix(text, "4;") {
		// An OSC 9 payload opening "4;" parses as a PROGRESS REPORT in every
		// terminal that supports both — a leading space keeps it a
		// notification. Guarded here at the mechanism, not by call-site
		// convention: the text is an arbitrary content digest.
		text = " " + text
	}
	seq := ansi.Notify(text) + "\a"
	out := m.notifyOut
	return func() tea.Msg {
		io.WriteString(out, seq)
		return nil
	}
}

func (m *model) Init() tea.Cmd {
	if m.oneShot && m.surf != nil && m.surf.spec.RefreshEvery > 0 {
		gen := m.surf.gen
		return tea.Tick(time.Duration(m.surf.spec.RefreshEvery)*time.Millisecond,
			func(time.Time) tea.Msg { return surfTickMsg{gen: gen} })
	}
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		widthChanged := msg.Width != m.width
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width)
		if m.widthO != nil {
			m.widthO.Store(int64(msg.Width))
		}
		if m.heightO != nil && msg.Height > 0 {
			m.heightO.Store(int64(msg.Height))
		}
		if widthChanged && m.flushTail != nil {
			// A width change reflows the screen and the renderer's relative
			// repaint ghosts the frame's TOP rows. Flush the staging tail
			// (content!) into scrollback first so the ghost surface is just
			// the separator. Run as a cmd: region locks + sends into the
			// mailbox, which must not happen from inside Update.
			flush := m.flushTail
			return m, func() tea.Msg { flush(); return nil }
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

	case progressMsg:
		m.progress = ProgressState(msg)
		return m, nil

	case notifyMsg:
		return m, m.notifyCmd(string(msg))

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case setCommandsMsg:
		m.commands = msg
		return m, nil

	case tea.PasteMsg:
		if m.surf != nil {
			if p := m.surf.spec.Panels[m.surf.focus]; p.Kind == PanelInput || m.surf.ps[m.surf.focus].editing {
				st := &m.surf.ps[m.surf.focus]
				flat := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(msg.Content)
				st.input, _ = st.input.Update(tea.PasteMsg{Content: strings.TrimSpace(flat)})
			}
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

	case busyDetailMsg:
		// Live sub-state on the current phase — the clock keeps running.
		if m.busy != nil {
			m.busy.detail = string(msg)
		}
		return m, nil

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

	case takeQueuedMsg:
		// Steering drain: pop the contiguous PREFIX of plain messages — a
		// slash command stops the take (commands run between turns, and
		// skipping past one would reorder it against the messages behind it).
		var taken []Input
		for len(m.queue) > 0 && !strings.HasPrefix(m.queue[0], "/") {
			taken = append(taken, m.makeInput(m.queue[0]))
			m.queue = m.queue[1:]
		}
		msg.reply <- taken
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
				// Re-filter against the new content: a live panel (/tools,
				// /debug) grows rows under an applied query, and they must be
				// judged by it too.
				st.rebuildView(p)
				st.syncCursor()
				if p.Kind == PanelView && st.search.query != "" {
					st.recollectHits(p, st.search.query)
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
		tab := k.Code == tea.KeyTab
		m.surfaceKey(k)
		if m.oneShot && m.surf == nil {
			return m, tea.Quit // one-shot: the surface WAS the program
		}
		if tab && m.surf != nil {
			// Force a full-frame repaint on tab switches: replacing one
			// panel's rows with another's exercises the renderer's cell diff
			// across CJK boundaries, which drops a wide rune's first cell on
			// some terminals (user-reported on Ghostty: the first CJK char of
			// the first row vanished after Delete→Resume until the cursor
			// moved). A sequential full redraw sidesteps the diff entirely,
			// and tab switches are rare, user-paced events.
			return m, tea.ClearScreen
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

	// ↑ on an EMPTY composer with messages queued pops the newest one back
	// for editing (LIFO, one per press) — the industry-consensus way to
	// manage the queue without touching the running turn (Claude Code ↑,
	// Codex Alt+Up, Gemini ↑). Popping IS the un-queue: edit and resubmit,
	// or Ctrl+U to discard. With text in the composer the arrows fall
	// through to history navigation as always.
	if k.Code == tea.KeyUp && len(m.queue) > 0 && strings.TrimSpace(m.ta.Value()) == "" {
		last := m.queue[len(m.queue)-1]
		m.queue = m.queue[:len(m.queue)-1]
		m.setDraft(last)
		return m, nil
	}

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

// makeInput builds the Display/Text pair: Text expands paste tags in full
// (what the model receives), Display expands them bounded (what the
// transcript echoes). The composer's own value keeps the tags.
func (m *model) makeInput(text string) Input {
	return Input{Display: expandPasteEcho(text, m.pastes), Text: expandPasteTags(text, m.pastes)}
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

	// An open inline "Other…" editor owns the keyboard. ESC closes JUST the
	// editor (back to the options — the user is refining an answer, not
	// declining the ask); Enter confirms the text: a Multi checks the row
	// and stays for more toggles, a single-select proceeds with it as the
	// answer (wizard advance / commit).
	if (p.Kind == PanelList || p.Kind == PanelMulti) && p.Custom && st.editing {
		otherIdx := len(p.Items)
		switch {
		case k.Mod == tea.ModCtrl && k.Code == 'c':
			m.closeSurface(TabbedResult{Cancelled: true})
		case k.Code == tea.KeyEscape:
			st.editing = false
			st.input.Blur()
		case k.Code == tea.KeyEnter:
			st.editing = false
			st.input.Blur()
			text := strings.TrimSpace(st.input.Value())
			if p.Kind == PanelMulti {
				st.checked[otherIdx] = text != ""
				return
			}
			if text == "" {
				return // nothing entered: stay on the options
			}
			if s.spec.EnterAdvances && s.focus < len(s.spec.Panels)-1 {
				s.setFocus(s.focus + 1)
				return
			}
			m.closeSurface(s.result())
		default:
			st.input, _ = st.input.Update(tea.KeyPressMsg(k))
		}
		return
	}

	// An open query field owns the keyboard for the same reason the Custom
	// editor above does — letters must type. Every keystroke re-applies the
	// query live (search.go): the list narrows, or the View rides to its first
	// hit, before Enter is ever pressed.
	if st.search.mode == searchTyping {
		switch {
		case k.Mod == tea.ModCtrl && k.Code == 'c':
			m.closeSurface(TabbedResult{Cancelled: true})
		case k.Code == tea.KeyEscape:
			st.searchClear(p) // ESC in the field leaves search entirely
		case k.Code == tea.KeyEnter:
			st.searchApply(p)
		default:
			st.search.input, _ = st.search.input.Update(tea.KeyPressMsg(k))
			st.searchLive(p)
		}
		return
	}

	// A View under an applied search hands n/p/q/Esc to the hit walker; every
	// other key (scrolling, c copy, g/G) still reaches the panel below.
	if p.Kind == PanelView && st.search.mode == searchApplied {
		switch {
		case k.Code == tea.KeyEscape:
			st.searchEdit(p)
			return
		case k.Text == "q":
			st.searchEdit(p)
			return
		case k.Text == "n":
			st.searchStep(1)
			return
		case k.Text == "p":
			st.searchStep(-1)
			return
		}
	}

	// Input panels own the keyboard: letters must type, not navigate. Only
	// the surface-level chords stay routed (Tab/Enter/Esc/Ctrl+C); the rest —
	// including textinput's emacs-style Ctrl bindings — feed the field.
	if p.Kind == PanelInput {
		switch {
		case k.Mod == tea.ModCtrl && k.Code == 'c':
			m.closeSurface(TabbedResult{Cancelled: true})
		case k.Code == tea.KeyTab:
			s.setFocus((s.focus + 1) % len(s.spec.Panels))
		case k.Code == tea.KeyEscape:
			m.closeSurface(TabbedResult{Cancelled: true})
		case k.Code == tea.KeyEnter:
			if s.spec.EnterAdvances && s.focus < len(s.spec.Panels)-1 {
				s.setFocus(s.focus + 1)
				return
			}
			m.closeSurface(s.result())
		default:
			st.input, _ = st.input.Update(tea.KeyPressMsg(k))
		}
		return
	}

	if k.Mod == tea.ModCtrl {
		// readline heritage: Ctrl+P/N mirror ↑↓, Ctrl+B/F mirror ←→ (paging).
		switch k.Code {
		case 'c':
			m.closeSurface(TabbedResult{Cancelled: true})
		case 'p':
			m.surfaceNav(p, st, -1)
		case 'n':
			m.surfaceNav(p, st, 1)
		case 'b':
			m.surfacePage(p, st, -1)
		case 'f':
			m.surfacePage(p, st, 1)
		}
		return
	}
	switch k.Code {
	case tea.KeyTab:
		s.setFocus((s.focus + 1) % len(s.spec.Panels))
		return
	case tea.KeyEscape:
		m.closeSurface(TabbedResult{Cancelled: true})
		return
	case tea.KeyEnter:
		if p.Kind == PanelBrowser && st.cursor >= 0 && st.cursor < len(st.entries) {
			e := st.entries[st.cursor]
			if e.isDir {
				st.setDir(p, e.path) // descend; do not submit
				return
			}
			st.chosen = e.path // file chosen; fall through to commit
		}
		if (p.Kind == PanelList || p.Kind == PanelMulti) && p.Custom && st.cursor == len(p.Items) &&
			strings.TrimSpace(st.input.Value()) == "" {
			// Empty Other: Enter opens the editor. With text already entered
			// the row behaves like any option (fall through to advance /
			// commit) — Space edits it.
			st.editing = true
			st.input.Focus()
			return
		}
		if s.spec.EnterAdvances && s.focus < len(s.spec.Panels)-1 {
			s.setFocus(s.focus + 1)
			return
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
		switch p.Kind {
		case PanelSlider:
			sliderStep(st, p, -1)
		case PanelSwitch:
			st.on = false
		default:
			m.surfacePage(p, st, -1)
		}
		return
	case tea.KeyRight:
		switch p.Kind {
		case PanelSlider:
			sliderStep(st, p, 1)
		case PanelSwitch:
			st.on = true
		default:
			m.surfacePage(p, st, 1)
		}
		return
	case tea.KeySpace:
		switch p.Kind {
		case PanelSwitch:
			st.on = !st.on
		case PanelList:
			if p.Custom && st.cursor == len(p.Items) {
				st.editing = true // Space (re)opens the editor on a single-select
				st.input.Focus()
				return
			}
		case PanelMulti:
			if p.Custom && st.cursor == len(p.Items) {
				if st.checked[st.cursor] {
					st.checked[st.cursor] = false // uncheck; the draft text stays
				} else {
					st.editing = true // check-by-editing (Enter confirms)
					st.input.Focus()
				}
				st.copied = false
				return
			}
			st.checked[st.cursor] = !st.checked[st.cursor]
		case PanelView:
			m.surfacePage(p, st, 1) // v1: Space pages a view forward
		}
		st.copied = false
		return
	}
	handled := true
	switch k.Text {
	case "/":
		if st.searchAvailable(p) {
			st.searchOpen(p)
			return
		}
		handled = false
	case "c":
		if p.Kind == PanelView {
			st.copied = copyToClipboard(stripSGRText(strings.Join(st.items, "\n"))) == nil
			return // keep the ✓ hint until another key
		}
		// On a row panel "c" was free, and an applied filter claims it: the
		// panel's own keys stay untouched, so this is the way back to the
		// whole list.
		if st.search.mode == searchApplied {
			st.searchClear(p)
			return
		}
	case "q":
		m.closeSurface(TabbedResult{Cancelled: true})
		return
	case " ":
		switch p.Kind {
		case PanelSwitch:
			st.on = !st.on
		case PanelMulti:
			st.checked[st.cursor] = !st.checked[st.cursor]
		case PanelView:
			m.surfacePage(p, st, 1) // v1: Space pages a view forward
		}
	case "b":
		if p.Kind == PanelView {
			m.surfacePage(p, st, -1)
		}
	case "j":
		m.surfaceNav(p, st, 1)
	case "k":
		m.surfaceNav(p, st, -1)
	case "h":
		switch {
		case p.Kind == PanelSlider:
			sliderStep(st, p, -1)
		case p.Kind == PanelSwitch:
			st.on = false
		case p.Kind == PanelView && !p.Wrap:
			if st.hoff > 0 {
				st.hoff--
			}
		default:
			m.surfacePage(p, st, -1)
		}
	case "l":
		switch {
		case p.Kind == PanelSlider:
			sliderStep(st, p, 1)
		case p.Kind == PanelSwitch:
			st.on = true
		case p.Kind == PanelView && !p.Wrap:
			st.hoff++
		default:
			m.surfacePage(p, st, 1)
		}
	case "g":
		if p.Kind == PanelSlider {
			st.value = nil
		} else {
			st.setViewPos(0)
			st.offset, st.hoff = 0, 0
		}
	case "G":
		switch p.Kind {
		case PanelSlider:
			v := p.Max
			st.value = &v
		case PanelView:
			st.offset = 1 << 30 // render clamps to the last page
		default:
			st.setViewPos(maxInt(0, len(st.view)-1))
		}
	default:
		handled = false
	}
	if handled {
		st.copied = false // any other handled key clears the copy confirmation
	}
}

// surfacePage moves by one visible page (v1 ←→ / Ctrl+B/F semantics). Row
// panels step through VISIBLE rows, so a filter's gaps are skipped rather than
// paged across.
func (m *model) surfacePage(p Panel, st *panelState, dir int) {
	step := st.rows
	if step < 1 {
		step = 1
	}
	switch p.Kind {
	case PanelList, PanelMulti, PanelPicker, PanelBrowser:
		st.setViewPos(st.viewPos() + dir*step)
	case PanelView:
		st.offset = maxInt(0, st.offset+dir*step) // upper bound clamped at render
	}
}

// surfaceNav moves the cursor / scroll of the focused panel.
func (m *model) surfaceNav(p Panel, st *panelState, dir int) {
	switch p.Kind {
	case PanelList, PanelMulti, PanelPicker, PanelBrowser:
		st.setViewPos(st.viewPos() + dir)
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
	if m.oneShot {
		var b strings.Builder
		m.surfCur = nil
		if m.surf != nil {
			m.renderSurface(&b)
		}
		view := tea.NewView(strings.TrimPrefix(b.String(), "\n"))
		if m.surfCur != nil {
			m.surfCur.Y-- // the trimmed leading newline
			view.Cursor = m.surfCur
		}
		return view
	}

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
	// Residue: rows a collapsed preview didn't cover, held until fresh lines
	// overwrite them (region.go) — the window height never dips when a widget
	// folds into its summary. Rendered BLANK: with block previews down to one
	// metered row, residue only ever holds stale widget chrome (spinner body
	// rows, the ⎿ status row), and showing that next to an injected user
	// block read as leaked tool output. The rows keep their height; their
	// content is noise.
	for range m.region.residue {
		b.WriteString("\n")
		rowsAbove++
	}
	if m.region.label != "" {
		// The label renders as supplied (callers pre-style it), so a streamed
		// tool call's header looks exactly like its final committed form.
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + sgrReset +
			" " + m.region.label + sgrReset + "\n")
		for _, line := range m.region.ptail {
			b.WriteString(faint + "  " + ansi.Truncate(line, maxInt(4, m.width-4), "…") + sgrReset + "\n")
		}
		rowsAbove += 1 + len(m.region.ptail)
		if !m.region.since.IsZero() {
			// The call preview's live status row: where the result's "⎿" line
			// will land, showing the caller's detail ("1.2k tokens") and the
			// elapsed time (spinner ticks re-render it), plus the cancel hint
			// until the tool's output replaces it. A paused clock (the user is
			// being consulted) freezes the figure where it stopped.
			status := "⎿ "
			if m.region.detail != "" {
				status += m.region.detail + " · "
			}
			elapsed := time.Since(m.region.since)
			if !m.region.pausedAt.IsZero() {
				elapsed = m.region.pausedAt.Sub(m.region.since)
			}
			status += timefmt.Elapsed(elapsed)
			if len(m.cancels) > 0 {
				status += " · ESC to cancel"
			}
			b.WriteString(faint + "  " + ansi.Truncate(status, maxInt(4, m.width-4), "…") + sgrReset + "\n")
			rowsAbove++
		}
	}

	// Spacer: one constant blank row between the content side (staging tail,
	// residue, previews) and everything input-side (queue, separator,
	// composer) — breathing room the user asked for. Constant height: it can
	// never bounce the frame.
	b.WriteString("\n")
	rowsAbove++

	// Type-ahead queue: dim "»" lines below the spacer, above the separator.
	// The block's bottom row carries the management hint (↑ pops the newest
	// queued message back into an empty composer for editing).
	if n := len(m.queue); n > 0 {
		shown := n
		if shown > queueShownMax {
			shown = queueShownMax
		}
		const hint = " · ↑ edit"
		for i := 0; i < shown; i++ {
			width := maxInt(4, m.width-4)
			if i == shown-1 && n == shown {
				width = maxInt(4, m.width-4-len(hint))
			}
			item := ansi.Truncate(m.queue[i], width, "…")
			if strings.HasPrefix(item, "/") {
				item = green + item + sgrReset + faint
			}
			if i == shown-1 && n == shown {
				item += hint
			}
			b.WriteString(faint + "» " + item + sgrReset + "\n")
		}
		if n > shown {
			fmt.Fprintf(&b, "%s  … +%d more%s%s\n", faint, n-shown, hint, sgrReset)
		}
		rowsAbove += shown
		if n > shown {
			rowsAbove++
		}
	}

	// The composer is wrapped by TWO separators; the bottom zone (below the
	// lower separator) holds exactly one of: an open interaction surface, the
	// slash-suggestion row, or the status line (surface > suggestions >
	// status). Swapping them is a content change on existing rows plus
	// below-composer growth/shrink — never a composer move.
	w := m.width
	if w < 1 {
		w = 80
	}
	b.WriteString(faint + strings.Repeat("─", w) + sgrReset + "\n")
	rowsAbove++

	b.WriteString(m.ta.View())

	b.WriteString("\n" + faint + strings.Repeat("─", w) + sgrReset)

	m.surfCur = nil
	switch {
	case m.surf != nil:
		// Interaction surface: replaces the status row; its vacated rows on
		// close die at the screen bottom where output self-heals them.
		m.renderSurface(&b)
	case m.suggestionRow() != "":
		b.WriteString("\n" + m.suggestionRow())
	default:
		b.WriteString("\n" + m.statusLine())
	}

	view := tea.NewView(b.String())
	view.WindowTitle = m.title
	view.ReportFocus = true // FocusMsg/BlurMsg gate the attention channel
	if m.progress != ProgressNone {
		view.ProgressBar = &tea.ProgressBar{State: progressBarState(m.progress), Value: 100}
	}
	if m.surfCur != nil {
		view.Cursor = m.surfCur // an input field owns the real cursor
	}
	if m.surf == nil {
		if c := m.ta.Cursor(); c != nil {
			c.Y += rowsAbove
			view.Cursor = c
		}
	}
	return view
}

// suggestionRow renders the slash-completion candidates while a bare "/"
// prefix is typed ("" when inactive). It occupies the status row's slot.
func (m *model) suggestionRow() string {
	base := m.sugBase
	if base == "" {
		base = m.ta.Value()
	}
	ms := matchCommands(m.commands, base)
	if len(ms) == 0 {
		return ""
	}
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
	return ansi.Truncate(row.String(), maxInt(4, m.width), "…")
}

// statusLine renders " model · ↑ in ↓ out · pct% / window", with the busy
// indicator appended while a turn is working. The busy state lives HERE — on
// a permanent frame row — precisely so its appearance/disappearance never
// changes the frame height (a transient busy row above the separator was the
// last remaining composer bounce).
func (m *model) statusLine() string {
	model := m.status.Model
	if model == "" {
		model = "—"
	}
	// What the session has cost so far: ↑ input, ↓ output. Each arrow shows
	// only once its side is non-zero, so a provider without token accounting
	// (and a session that hasn't spent anything yet) drops the segment
	// instead of printing zeros. The cache share qualifies ↑ instead of
	// standing beside it — it is part of that input, not a third quantity.
	tokens := ""
	if m.status.InTokens > 0 {
		tokens = "↑ " + tokfmt.Tokens(m.status.InTokens)
		if m.status.CacheHitPct > 0 {
			tokens += fmt.Sprintf(" (%.0f%% cached)", m.status.CacheHitPct)
		}
	}
	if m.status.OutTokens > 0 {
		if tokens != "" {
			tokens += " "
		}
		tokens += "↓ " + tokfmt.Tokens(m.status.OutTokens)
	}
	// How full the context window is — the auto-compaction threshold's only
	// visible warning, hence the hue shift as it fills. A leading ≈ marks a
	// locally estimated figure.
	ctx, ctxHue := "", green
	if m.status.CtxWindow > 0 {
		pct := m.status.CtxUsed * 100 / m.status.CtxWindow
		approx := ""
		if m.status.Estimated {
			approx = "≈"
		}
		ctx = fmt.Sprintf("%s%d%% / %s", approx, pct, tokfmt.Tokens(m.status.CtxWindow))
		ctxHue = usageHue(pct)
	}

	frame, tail := "", ""
	if m.busy != nil {
		frame = spinnerFrames[m.spin%len(spinnerFrames)]
		tail = " " + m.busy.label
		if m.busy.detail != "" {
			tail += " · " + m.busy.detail
		}
		if elapsed := time.Since(m.busy.since); elapsed >= 2*time.Second {
			tail += "  " + timefmt.Elapsed(elapsed)
		}
		if len(m.cancels) > 0 {
			tail += " (ESC to cancel)"
		}
	}

	// Recording is a MODE, not a figure: it sits after the numbers and
	// before the busy indicator, and it is the one segment that stays put
	// when the line is truncated (see below) — a mode you cannot see is how
	// an unfolded transcript gets blamed on the model.
	dbg := ""
	if m.status.Debug {
		dbg = "debug"
	}

	plain := "  " + model
	for _, seg := range []string{tokens, ctx, dbg} {
		if seg != "" {
			plain += " · " + seg
		}
	}
	if m.busy != nil {
		plain += " · " + frame + tail
	}
	if textwidth.StringWidth(plain) > m.width {
		// Truncation eats the tail, which is where the mode marker sits —
		// so re-append it. A narrow terminal is exactly where a silently
		// changed layout is hardest to explain.
		line := ansi.Truncate(plain, maxInt(4, m.width), "…")
		if dbg != "" {
			if room := m.width - textwidth.StringWidth(" · "+dbg); room > 4 {
				line = ansi.Truncate(plain, room, "…") + " · " + dbg
			}
		}
		return faint + line + sgrReset
	}
	out := "  " + cyan + faint + model + sgrReset
	if tokens != "" {
		out += faint + " · " + sgrReset + green + faint + tokens + sgrReset
	}
	if ctx != "" {
		out += faint + " · " + sgrReset + ctxHue + faint + ctx + sgrReset
	}
	if dbg != "" {
		out += faint + " · " + sgrReset + yellow + faint + dbg + sgrReset
	}
	if m.busy != nil {
		out += faint + " · " + sgrReset + cyan + frame + sgrReset + faint + tail + sgrReset
	}
	return out
}

// usageHue warms the context figure as the window fills: green while there is
// room, yellow past 70%, red past 90% — the 80% auto-compaction prompt should
// never arrive as a surprise.
func usageHue(pct int) string {
	switch {
	case pct > 90:
		return red
	case pct > 70:
		return yellow
	default:
		return green
	}
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

// pasteEchoMaxLines caps how much of a pasted block the sent-message echo
// shows. The composer keeps the tag (a thousand-line blob is unusable to
// edit, and ↑-recall re-expands on submit), but the transcript should show
// what was actually sent — bounded, so a big paste does not bury the reply.
const pasteEchoMaxLines = 20

// pasteTagRe matches the composer paste tags for expansion on submit.
var pasteTagRe = regexp.MustCompile(`\[#(\d+)[^\]]*\]`)

// expandPasteTags replaces every stored-paste tag with its full content.
func expandPasteTags(text string, pastes []string) string {
	return replacePasteTags(text, pastes, func(content string) string { return content })
}

// expandPasteEcho expands tags for the transcript echo, trimming any paste
// longer than pasteEchoMaxLines to its head plus a count of what follows.
func expandPasteEcho(text string, pastes []string) string {
	return replacePasteTags(text, pastes, func(content string) string {
		lines := strings.Split(content, "\n")
		if len(lines) <= pasteEchoMaxLines {
			return content
		}
		return strings.Join(lines[:pasteEchoMaxLines], "\n") +
			fmt.Sprintf("\n… +%d more lines", len(lines)-pasteEchoMaxLines)
	})
}

// replacePasteTags rewrites every stored-paste tag through render; an index
// with no stored paste (a literal "[#7 …]" the user typed) is left alone.
func replacePasteTags(text string, pastes []string, render func(content string) string) string {
	return pasteTagRe.ReplaceAllStringFunc(text, func(tag string) string {
		mm := pasteTagRe.FindStringSubmatch(tag)
		idx, err := strconv.Atoi(mm[1])
		if err != nil || idx < 1 || idx > len(pastes) {
			return tag
		}
		return render(pastes[idx-1])
	})
}
