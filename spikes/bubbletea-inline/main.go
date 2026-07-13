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
	mode     uiMode
	selIdx   int
	selItems []string
	streamed int // lines inserted so far — shown in the status line so each
	// insert CHANGES the view (defeats flush's viewEquals early-return; see
	// the repaintMsg comment)
}

func newModel() model {
	ta := textarea.New()
	ta.SetVirtualCursor(false) // REAL terminal cursor — the IME probe hinges on this
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetPromptFunc(2, func(textarea.PromptInfo) string { return cyan + "❯ " + stReset })
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
		tea.Println("用中文输入法在下方输入(看候选框位置);/model + Enter 开选择器(流不停);Ctrl+C 退出。"),
	)
}

// repaintMsg forces a no-op render after each Println: bubbletea v2's
// insertAbove leaves the physical cursor at the frame's top-left and no render
// follows (tea.go printLineMessage case), which would yank the IME preedit
// anchor away from the composer during streaming. The follow-up render diffs
// the cursor position and emits a MoveTo, shrinking the wrong-cursor window to
// one message-loop iteration.
type repaintMsg struct{}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case repaintMsg:
		m.streamed++ // view changes -> flush runs -> cursor restored
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.ta.SetWidth(msg.Width)
		return m, nil

	case tea.KeyPressMsg:
		k := msg.Key()
		if k.Mod == tea.ModCtrl && (k.Code == 'c' || k.Code == 'd') {
			return m, tea.Quit
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
				m.mode = composing
			case tea.KeyEnter:
				choice := m.selItems[m.selIdx]
				m.mode = composing
				return m, tea.Println(faint + "model switched to " + choice + stReset)
			}
			return m, nil // selector owns the keys; composer stays visible below
		}
		if k.Code == tea.KeyEnter {
			input := strings.TrimSpace(m.ta.Value())
			m.ta.Reset()
			switch input {
			case "":
				return m, nil
			case "/model":
				m.mode = selecting
				m.selIdx = 0
				return m, nil
			default:
				// Echo like the real app's user block, into native scrollback.
				return m, tea.Println(revOn + " ❯ " + input + " " + stReset)
			}
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	var b strings.Builder
	rowsAbove := 0 // frame rows above the textarea, to offset the real cursor

	if m.mode == selecting {
		b.WriteString(faint + "── /model — 流不停,选择器照开(↑↓ Enter,ESC 取消) ──" + stReset + "\n")
		for i, it := range m.selItems {
			if i == m.selIdx {
				b.WriteString(cyan + "▸ " + it + stReset + "\n")
			} else {
				b.WriteString("  " + it + "\n")
			}
		}
		rowsAbove += 1 + len(m.selItems)
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
	if os.Getenv("NOSTREAM") == "" {
		go stream(p, done)
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
