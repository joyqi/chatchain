// Command btinline-spike is the DECISIVE probe for the "full bubbletea v2"
// path: an INLINE (non-alt-screen) Program that owns only a small bottom frame
// (separator + live status + composer) while history flows above it into
// native scrollback via Println — the terminal's own scrolling is never taken
// over. It exists to answer the one question that can veto the whole path:
//
//	Does a Chinese IME work first-class in a bubbletea v2 composer with the
//	REAL terminal cursor (textarea.SetVirtualCursor(false) + tea.View.Cursor)?
//
// Run it in a real terminal (Ghostty AND Terminal.app; also inside tmux):
//
//	cd spikes/bubbletea-inline && go run .
//
// Probe checklist while lines stream above:
//  1. Type Chinese with an IME: does the candidate window appear AT the
//     cursor (not bottom-left / wrong row)? Does committed text land right?
//  2. CJK width in the composer: no misalignment, cursor column correct.
//  3. Type "/model" + Enter: a selector opens INSIDE the frame while the
//     stream KEEPS flowing above — the capability readline cannot give us.
//  4. Resize the window mid-stream: frame re-anchors, no ghost rows.
//  5. Terminal.app (no sync-output mode 2026): watch for flicker.
//
// Plain text + Enter echoes as a reversed "user block" line into scrollback.
// Ctrl+C quits. This spike has its own go.mod and never touches the main app.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Raw ANSI styles — keep the spike dependency-light and deterministic.
const (
	faint   = "\x1b[2m"
	cyan    = "\x1b[36m"
	green   = "\x1b[32m"
	revOn   = "\x1b[7m"
	stReset = "\x1b[0m"
)

type uiMode int

const (
	composing uiMode = iota
	selecting        // "/model" selector open INSIDE the frame; stream keeps flowing
)

type model struct {
	ta       textarea.Model
	width    int
	height   int
	mode     uiMode
	selIdx   int
	selItems []string
	streamed int // lines inserted so far — shown in the status line so each
	// insert CHANGES the view (defeats flush's viewEquals early-return; see
	// the repaintMsg comment)
	inserted int // ALL lines printed above (stream + echoes + results)

	// Live region ("/table" demo): dynamic in-flight content lives in the
	// FRAME (repainted every frame) while committed history above is
	// append-only. This is where the real app's spinner and the table/list
	// StreamView previews map to in the bubbletea architecture.
	thinking bool     // spinner phase
	spin     int      // spinner animation frame
	live     []string // partial table rows, streaming into the frame

	// Type-ahead queue (ui-owned, per the architecture): submits during an
	// active turn queue here, rendered as dim "»" lines ABOVE the separator
	// (content side). The turn's end drains them in order; ESC/Ctrl+C cancels
	// the turn and restores the queue into the composer draft (joined by \n).
	turnActive bool
	turnGen    int // generation guard: cancelling bumps it, stale ticks no-op
	queue      []string
}

// queueShownMax caps the visible queued lines; beyond it a "+N more" row.
const queueShownMax = 3

// queueRows returns how many frame rows the queue block occupies.
func (m *model) queueRows() int {
	n := len(m.queue)
	if n == 0 {
		return 0
	}
	shown := n
	if shown > queueShownMax {
		return queueShownMax + 1 // +1 for the "+N more" line
	}
	return shown
}

// spinnerFrames animate the frame-resident spinner (the old stderr
// cursor-control spinner cannot exist in this architecture).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// The live-region demo mirrors the REAL app's StreamView semantics
// (chat/markdown.go): while a table block buffers, show a spinner header plus
// a rolling window of the last previewWindow RAW SOURCE lines (dim); when the
// source is complete, clear the preview and render the final table ONCE.
const previewWindow = 3

// tableSrc is the raw markdown source that "streams in" line by line.
var tableSrc = []string{
	"| model | ctx | price |",
	"| --- | --- | --- |",
	"| gpt-4o | 128k | $2.50 |",
	"| claude-opus | 200k | $3.00 |",
	"| gemini-pro | 1m | $1.20 |",
	"| 中文模型 | 1m | $0.80 |",
	"| grok | 128k | $2.00 |",
}

// tableRendered is the final one-shot render emitted after the source ends.
var tableRendered = []string{
	"┌─────────────┬──────┬───────┐",
	"│ model       │ ctx  │ price │",
	"├─────────────┼──────┼───────┤",
	"│ gpt-4o      │ 128k │ $2.50 │",
	"│ claude-opus │ 200k │ $3.00 │",
	"│ gemini-pro  │ 1m   │ $1.20 │",
	"│ 中文模型    │ 1m   │ $0.80 │",
	"│ grok        │ 128k │ $2.00 │",
	"└─────────────┴──────┴───────┘",
}

func newModel() model {
	ta := textarea.New()
	ta.SetVirtualCursor(false) // REAL terminal cursor — the IME probe hinges on this
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetPromptFunc(2, func(pi textarea.PromptInfo) string {
		if pi.LineNumber == 0 {
			return cyan + "❯ " + stReset
		}
		return "  " // soft-wrapped continuation rows align under the input
	})
	ta.SetWidth(80) // after prompt/line-number config (see textarea.SetWidth doc)
	ta.Focus()
	return model{
		ta:       ta,
		width:    80,
		selItems: []string{"gpt-4o", "claude-opus", "gemini-pro", "中文选项测试", "取消"},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.Println(faint+"── bubbletea v2 inline spike ──"+stReset),
		tea.Println("发消息=模拟回合(流式);回合中继续提交=排队(» 行);ESC/Ctrl+C=打断(队列退回草稿);/model /table 照常。"),
	)
}

// repaintMsg forces a no-op render after each Println: bubbletea v2's
// insertAbove leaves the physical cursor at the frame's top-left and no render
// follows (tea.go printLineMessage case), which would yank the IME preedit
// anchor away from the composer during streaming. The follow-up render diffs
// the cursor position and emits a MoveTo, shrinking the wrong-cursor window to
// one message-loop iteration.
type repaintMsg struct{}

// Live-region demo messages: spinner ticks while "thinking", table rows arrive
// one by one into the frame's live region, then the block commits — one
// multi-line Println into immutable scrollback — and the live region clears.
type (
	spinTickMsg    struct{}
	tableRowMsg    struct{ i int }
	tableCommitMsg struct{}
)

// Turn simulation messages: a submitted message runs a fake turn (thinking →
// N streamed lines → done); done drains the type-ahead queue.
type (
	streamLineMsg struct{ gen, n int }
	turnDoneMsg   struct{ gen int }
	drainMsg      struct{}
)

const turnLines = 10 // streamed lines per fake turn

// NOTE on frame shrinks: bubbletea's inline frame is TOP-anchored, so a shrink
// leaves dead rows at the BOTTOM of the old frame extent (= the screen bottom
// once the frame has reached it). With the interaction area placed BELOW the
// composer (and the composer collapse likewise shedding its lowest rows), the
// dead rows always land at the screen bottom, where subsequent output consumes
// them one insert at a time — the frame walks back down and history stays
// contiguous. This layout choice DELETED the whole re-anchor state machine
// (scheduleShrink / pendingBlanks / deferred filler) an above-the-composer
// interaction area required. Cost: the composer floats above the bottom until
// the next output refills the gap — transient, and it never moves on close.

// maxComposerRows caps the composer's dynamic growth; longer input scrolls
// inside the textarea.
const maxComposerRows = 5

// scheduleShrink handles ANY frame shrink (selector closing, multi-row
// composer collapsing on submit/delete): the frame is top-anchored, so on a
// full screen a shrink strands the composer above the bottom. Defer `result`
// (optional) plus blank filler so the total insert count equals `delta` —
// each insertAbove pushes a not-at-bottom frame down one row — restoring the
// bottom anchor in one hop after the shrunk frame has flushed. On a
// not-yet-full screen (frame not at the bottom) the result prints directly.
// closeSelector returns to composing. With the interaction area BELOW the
// composer, closing needs NO re-anchor machinery: the vacated rows sit at the
// screen bottom and subsequent output consumes them one insert at a time (each
// insertAbove pushes a not-at-bottom frame down one row), keeping history
// contiguous — no blank gaps, no filler. Only a one-line outcome is committed
// as the interaction record; then the type-ahead queue drains.
func (m *model) closeSelector(chosen string, cancelled bool) tea.Cmd {
	m.mode = composing
	result := faint + "✗ /model cancelled" + stReset
	if !cancelled {
		result = faint + "model switched to " + chosen + stReset
	}
	m.inserted++
	return tea.Batch(
		tea.Println(result),
		tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return drainMsg{} }),
	)
}

// streamSrc are the fake reply lines for the turn simulation.
var streamSrc = []string{
	"The quick brown fox jumps over the lazy dog and keeps running.",
	"这是一段中文流式回复,用来观察排队消息与 composer 的交互。",
	"Mixed 中英文 line: numbers 1234567890, symbols · — √ π ≈, done.",
	"更长的一行:包含标点符号、破折号——省略号……以及全角字符,ＡＢＣ。",
	"emoji test 😀🚀🌟 and 中文 mixed with ASCII tail xyz.",
}

// dispatch routes one input (a fresh submit or a drained queue item): commands
// open their surface, messages echo a user block and start a fake turn.
func (m *model) dispatch(input string) tea.Cmd {
	switch input {
	case "/model":
		m.mode = selecting
		m.selIdx = 0
		return nil
	case "/table":
		m.turnActive = true
		m.thinking = true
		m.spin = 0
		return tea.Batch(
			tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} }),
			tea.Tick(900*time.Millisecond, func(time.Time) tea.Msg { return tableRowMsg{i: 0} }),
		)
	default:
		m.inserted++
		return tea.Batch(
			tea.Println(revOn+" ❯ "+input+" "+stReset),
			m.startTurn(),
		)
	}
}

// startTurn begins the fake reply: thinking spinner, then streamed lines.
func (m *model) startTurn() tea.Cmd {
	m.turnActive = true
	m.thinking = true
	m.spin = 0
	gen := m.turnGen
	return tea.Batch(
		tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} }),
		tea.Tick(600*time.Millisecond, func(time.Time) tea.Msg { return streamLineMsg{gen: gen, n: 0} }),
	)
}

// cancelTurn implements the interrupt contract: stop the stream (generation
// bump), restore the queue into the composer draft (joined by newlines, before
// any half-typed draft), and print an "interrupted" marker. Shed rows (queue,
// preview) die at the screen bottom and self-heal on the next output.
func (m *model) cancelTurn() tea.Cmd {
	m.turnGen++ // stale stream/table ticks become no-ops
	m.turnActive = false
	m.thinking = false
	m.live = nil

	if len(m.queue) > 0 {
		draft := strings.Join(m.queue, "\n")
		if cur := m.ta.Value(); strings.TrimSpace(cur) != "" {
			draft += "\n" + cur
		}
		m.queue = nil
		m.ta.SetValue(draft)
		rows := m.ta.LineCount() // one row per restored logical line (short lines)
		if rows > maxComposerRows {
			rows = maxComposerRows
		}
		m.ta.SetHeight(rows)
		m.ta.MoveToEnd()
	}
	m.inserted++
	return tea.Println(faint + "✗ interrupted" + stReset)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case repaintMsg:
		m.streamed++ // view changes -> flush runs -> cursor restored
		m.inserted++
		return m, nil

	case spinTickMsg:
		if !m.thinking && len(m.live) == 0 {
			return m, nil // no animated header on screen — stop the tick chain
		}
		m.spin++ // drives both the Thinking spinner and the preview header
		return m, tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })

	case tableRowMsg:
		if !m.turnActive {
			return m, nil // cancelled — stale table tick
		}
		m.thinking = false // first source line ends the "waiting" spinner phase
		m.live = append(m.live, tableSrc[msg.i])
		if msg.i+1 < len(tableSrc) {
			return m, tea.Tick(350*time.Millisecond, func(time.Time) tea.Msg { return tableRowMsg{i: msg.i + 1} })
		}
		return m, tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tableCommitMsg{} })

	case streamLineMsg:
		if msg.gen != m.turnGen {
			return m, nil // cancelled turn — stale tick
		}
		m.thinking = false
		line := streamSrc[msg.n%len(streamSrc)]
		next := tea.Tick(140*time.Millisecond, func(time.Time) tea.Msg { return streamLineMsg{gen: msg.gen, n: msg.n + 1} })
		if msg.n+1 >= turnLines {
			next = tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return turnDoneMsg{gen: msg.gen} })
		}
		return m, tea.Batch(
			tea.Sequence(tea.Println(line), func() tea.Msg { return repaintMsg{} }),
			next,
		)

	case turnDoneMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
		m.turnActive = false
		return m, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return drainMsg{} })

	case drainMsg:
		// Drain the type-ahead queue: dispatch the next item between turns.
		if m.turnActive || m.mode == selecting || len(m.queue) == 0 {
			return m, nil
		}
		item := m.queue[0]
		m.queue = m.queue[1:]
		// The dispatched item's UserBlock/print pushes balance the shed queue
		// row, so no explicit re-anchor is needed for the common case.
		return m, m.dispatch(item)

	case tableCommitMsg:
		if !m.turnActive {
			return m, nil // cancelled — stale table tick
		}
		// Source complete: clear the preview and render the final table ONCE —
		// a single multi-line Println into scrollback, exactly the real
		// StreamView.Done + flushTable sequence. Its pushes walk the frame back
		// down over the shed preview rows; history stays contiguous.
		final := green + strings.Join(tableRendered, "\n") + stReset
		m.live = nil
		m.turnActive = false
		m.inserted += len(tableRendered)
		return m, tea.Batch(
			tea.Sequence(tea.Println(final), func() tea.Msg { return repaintMsg{} }),
			tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg { return drainMsg{} }),
		)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width)
		// Resize + reflow is the structural weak spot of ANY inline renderer:
		// rewrapping terminals (Ghostty, Terminal.app) rewrap long painted
		// frame rows, shifting the frame under a renderer that only tracks
		// positions RELATIVELY — the repaint lands off-by-N and displaced rows
		// leak into scrollback as orphans (tmux doesn't rewrap and stays
		// clean). Deliberately do NOTHING extra here: an earlier auto-close-
		// the-selector mitigation made it WORSE — the shrink erase + re-anchor
		// filler all executed against the desynced position model, orphaning
		// the separator/status rows too. Any churn during the desync window
		// amplifies leakage; a stray row or two in scrollback after a resize
		// is the accepted cost (all inline renderers share it).
		return m, nil

	case tea.KeyPressMsg:
		k := msg.Key()
		if k.Mod == tea.ModCtrl && (k.Code == 'c' || k.Code == 'd') {
			if m.turnActive {
				return m, m.cancelTurn() // first Ctrl+C cancels the turn (queue → draft)
			}
			return m, tea.Quit
		}
		if k.Code == tea.KeyEscape && m.mode == composing && m.turnActive {
			return m, m.cancelTurn() // ESC cancels the streaming turn
		}
		if m.mode == selecting {
			switch k.Code {
			case tea.KeyUp:
				if m.selIdx > 0 {
					m.selIdx--
				}
			case tea.KeyDown:
				if m.selIdx < len(m.selItems)-1 {
					m.selIdx++
				}
			case tea.KeyEscape:
				return m, m.closeSelector("", true)
			case tea.KeyEnter:
				return m, m.closeSelector(m.selItems[m.selIdx], false)
			}
			return m, nil // selector owns the keys; composer stays visible below
		}
		if k.Code == tea.KeyEnter {
			input := strings.TrimSpace(m.ta.Value())
			m.ta.Reset()
			m.ta.SetHeight(1) // collapse sheds bottom rows — self-healing, no re-anchor
			if input == "" {
				return m, nil
			}
			if m.turnActive {
				// Type-ahead: a submit during an active turn queues (ui-owned).
				m.queue = append(m.queue, input)
				return m, nil
			}
			return m, m.dispatch(input)
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	// Dynamic composer height: Enter submits, so the content is one logical
	// line and LineInfo().Height is its soft-wrapped display rows. Grow the
	// textarea (the frame grows upward for free); a shrink (deleting back
	// under a wrap boundary) re-anchors like any other frame shrink.
	// A restored multi-logical-line draft (queue → draft after cancel) keeps
	// its explicit height; the single-line dynamic sizing below would squash it
	// (LineInfo().Height covers only the CURRENT logical line).
	if m.mode == composing && m.ta.LineCount() <= 1 {
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
			if rows > old {
				// The textarea's viewport may be left scrolled from when it
				// was shorter (repositionView only keeps the cursor visible,
				// it never scrolls back). If the whole content now fits, snap
				// the view to the top, preserving the cursor column.
				if contentRows <= maxComposerRows && m.ta.ScrollYOffset() > 0 {
					col := m.ta.Column()
					m.ta.MoveToBegin()
					m.ta.SetCursorColumn(col)
				}
			}
		}
	}
	return m, cmd
}

func (m model) View() tea.View {
	var b strings.Builder
	rowsAbove := 0 // frame rows above the textarea, to offset the real cursor

	// Live region: dynamic in-flight content (spinner / streaming block
	// preview) lives in the frame, repainted every render — the bubbletea
	// mapping of the real app's spinner and table/list StreamView.
	if m.thinking {
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + stReset + faint + " Thinking..." + stReset + "\n")
		rowsAbove++
	}
	if len(m.live) > 0 {
		// Real StreamView semantics: spinner header + a ROLLING window of the
		// last previewWindow RAW SOURCE lines (dim), repainted each frame.
		// Once the window fills, the frame height stays CONSTANT until commit.
		b.WriteString(cyan + spinnerFrames[m.spin%len(spinnerFrames)] + stReset + faint + " rendering table…" + stReset + "\n")
		tail := m.live
		if len(tail) > previewWindow {
			tail = tail[len(tail)-previewWindow:]
		}
		for _, r := range tail {
			b.WriteString(faint + "  " + r + stReset + "\n")
		}
		rowsAbove += 1 + len(tail)
	}

	// Type-ahead queue: dim "»" lines ABOVE the separator (content side, per
	// design) — inputs waiting to become history the moment the turn ends.
	if n := len(m.queue); n > 0 {
		shown := n
		if shown > queueShownMax {
			shown = queueShownMax
		}
		for i := 0; i < shown; i++ {
			item := ansi.Truncate(m.queue[i], max(4, m.width-4), "…")
			if strings.HasPrefix(item, "/") {
				item = green + item + stReset
			}
			b.WriteString(faint + "» " + stReset + faint + item + stReset + "\n")
		}
		if n > shown {
			b.WriteString(faint + fmt.Sprintf("  … +%d more", n-shown) + stReset + "\n")
		}
		rowsAbove += m.queueRows()
	}

	w := m.width
	if w < 1 {
		w = 80
	}
	b.WriteString(faint + strings.Repeat("─", w) + stReset + "\n")
	draft := len([]rune(m.ta.Value()))
	pct := (12000 + draft) * 100 / 128000
	b.WriteString(fmt.Sprintf("  %s%sfake-4o%s %s·%s %s%sctx ≈%d / 128k (%d%%)%s %s· streamed %d%s\n",
		cyan, faint, stReset, faint, stReset, green, faint, 12000+draft, pct, stReset, faint, m.streamed, stReset))
	rowsAbove += 2

	b.WriteString(m.ta.View())

	// Interaction area BELOW the composer (the user's layout insight): shell
	// convention (completion menus render under the prompt), the composer
	// never moves when the surface closes, and the vacated rows die at the
	// screen bottom where output self-heals them — no re-anchor machinery.
	if m.mode == selecting {
		b.WriteString("\n" + faint + "── /model(↑↓ Enter,ESC 取消)──" + stReset)
		for i, it := range m.selItems {
			if i == m.selIdx {
				b.WriteString("\n" + cyan + "▸ " + it + stReset)
			} else {
				b.WriteString("\n  " + it)
			}
		}
	}

	v := tea.NewView(b.String())
	if m.mode == composing {
		if c := m.ta.Cursor(); c != nil {
			c.Y += rowsAbove // offset by the frame rows above the textarea
			v.Cursor = c
		}
	}
	return v
}

func main() {
	p := tea.NewProgram(newModel())
	done := make(chan struct{})
	if os.Getenv("ENDLESS") != "" {
		go stream(p, done) // legacy jitter-probe workload; turns are the default now
	}
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "spike:", err)
		os.Exit(1)
	}
	close(done)
}

// stream pushes line-buffered mixed CJK/English text ABOVE the program via
// Program.Println — the same workload as the readline spike, for comparison.
func stream(p *tea.Program, done <-chan struct{}) {
	time.Sleep(300 * time.Millisecond) // let the program start
	lines := []string{
		"The quick brown fox jumps over the lazy dog and keeps running.",
		"这是一段中文流式文本,用来观察输出时底部 frame 会不会漂移。",
		"Mixed 中英文 line: numbers 1234567890, symbols · — √ π ≈, done.",
		"更长的一行:包含标点符号、破折号——省略号……以及全角字符,ＡＢＣ。",
		"Line %d streaming — tea.Println 插入原生 scrollback,frame 不重画。",
		"emoji test 😀🚀🌟 and 中文 mixed with ASCII tail xyz.",
	}
	i := 0
	for {
		select {
		case <-done:
			return
		default:
		}
		line := lines[i%len(lines)]
		if strings.Contains(line, "%d") {
			line = fmt.Sprintf(line, i)
		}
		p.Println(line)
		p.Send(repaintMsg{}) // restore the real cursor right after the insert
		i++
		d := 140 * time.Millisecond
		if i%7 == 0 {
			d = 30 * time.Millisecond // occasional burst
		}
		select {
		case <-done:
			return
		case <-time.After(d):
		}
	}
}
