// Command typeahead is a throwaway spike to answer one question before building
// type-ahead for real: does the three-line input composer (separator + live
// status + editable "❯ " line) jitter when streaming output is pushed above it
// concurrently, at per-line frequency, with mixed CJK/English text?
//
// It reuses the vendored readline exactly as the real app does: the composer is
// a multi-line prompt, a keystroke Listener rebuilds the live status and forces a
// full repaint, and a background goroutine streams line-buffered fake output into
// rl.Stdout() (readline's "write above the prompt" path) while you type.
//
// Run it in a real terminal (it needs a TTY):
//
//	go run ./spikes/typeahead
//
// Then type — including Chinese — while lines stream above. Watch for: the "❯ "
// cursor drifting, the status numbers tearing, CJK characters misaligning, or
// the whole three-line block flickering/duplicating. Ctrl+C or Ctrl+D quits.
//
// If it stays stable → the lightweight rl.Stdout() type-ahead is viable. If it
// jitters badly (especially on CJK) → fall back to a read-only composer during
// streaming, or drop it. This spike touches no production code.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"chatchain/internal/promptui"
	"chatchain/internal/readline"

	"github.com/fatih/color"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

// spikeSlash are the fake slash commands the completer offers, mirroring the
// real app's shape (selectors + viewers).
var spikeSlash = []string{"/model", "/status", "/debug", "/file", "/tools"}

func isSlash(input string) bool {
	for _, c := range spikeSlash {
		if input == c || strings.HasPrefix(input, c+" ") {
			return true
		}
	}
	return false
}

// spikeCompleter offers the slash commands on Tab (readline opens the menu; the
// injected Tab from slashReader triggers it automatically after "/").
type spikeCompleter struct{}

func (c *spikeCompleter) Do(line []rune, pos int) ([][]rune, int) {
	text := string(line[:pos])
	if !strings.HasPrefix(text, "/") || strings.Contains(text, " ") {
		return nil, 0
	}
	var cands [][]rune
	for _, cmd := range spikeSlash {
		full := cmd + " "
		if strings.HasPrefix(full, text) {
			cands = append(cands, []rune(full[len(text):]))
		}
	}
	return cands, len([]rune(text))
}

// slashReader injects a Tab after a "/" typed on an empty line so the completion
// menu auto-opens — the same trick as the real app's slashTriggerReader.
type slashReader struct {
	r     io.Reader
	empty *atomic.Bool
	queue []byte
}

func (s *slashReader) Read(p []byte) (int, error) {
	if len(s.queue) > 0 {
		n := copy(p, s.queue)
		s.queue = s.queue[n:]
		return n, nil
	}
	n, err := s.r.Read(p)
	if n == 0 {
		return n, err
	}
	var out []byte
	for i := 0; i < n; i++ {
		b := p[i]
		out = append(out, b)
		switch {
		case b == '\r' || b == '\n' || b == 0x03:
			s.empty.Store(true)
		case b == '/' && s.empty.Load():
			out = append(out, '\t')
			s.empty.Store(false)
		}
	}
	m := copy(p, out)
	if m < len(out) {
		s.queue = append(s.queue, out[m:]...)
		return m, nil
	}
	return m, err
}

// commandPainter colorizes a leading slash command (green when complete).
func commandPainter(line []rune, _ int) []rune {
	if len(line) == 0 || line[0] != '/' {
		return line
	}
	end := len(line)
	for i, r := range line {
		if r == ' ' {
			end = i
			break
		}
	}
	out := append([]rune{}, []rune(color.New(color.FgGreen).Sprint(string(line[:end])))...)
	return append(out, line[end:]...)
}

// runModalCommand is the "commands are modal" approach: pause the background
// stream, run a promptui selector/viewer (which takes the whole screen via its
// own readline+screenbuf), then resume the stream. This probes whether a
// screenbuf TUI and the type-ahead stream can share the terminal by taking turns.
func runModalCommand(input string, paused *atomic.Bool) {
	paused.Store(true)
	defer paused.Store(false)
	// Let any in-flight stream write finish before the TUI grabs the screen.
	time.Sleep(60 * time.Millisecond)

	switch {
	case strings.HasPrefix(input, "/status"), strings.HasPrefix(input, "/tools"):
		v := promptui.Viewer{
			Label:  input + "  (modal — stream paused)",
			Lines:  []string{"model: fake-4o", "context: 12k / 128k (9%)", "这是查看器,后台流已暂停", "q / ESC 退出后流恢复"},
			Height: 10,
		}
		_ = v.Run()
	default: // /model, /file, /debug → a Select
		pr := promptui.Select{
			Label:        input + "  (modal — stream paused)",
			Items:        []string{"gpt-4o", "claude-opus", "gemini-pro", "中文选项测试", "取消"},
			Size:         6,
			HideSelected: true,
		}
		_, _, _ = pr.Run()
	}
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

var (
	sepStyle    = color.New(color.Faint)
	modelStyle  = color.New(color.FgCyan, color.Faint)
	ctxStyle    = color.New(color.FgGreen, color.Faint)
	markerStyle = color.New(color.FgCyan, color.Bold)
)

// composerPrompt builds the same shape as the real composer: a full-width
// separator, a status line indented to the input column with per-field faint
// hues, and the "❯ " marker on its own final line (so its column is stable).
func composerPrompt(draftRunes int) string {
	w := termWidth()
	sep := sepStyle.Sprint(strings.Repeat("─", w))
	// draftRunes stands in for the live draft-token count so the status changes
	// on every keystroke, forcing a repaint exactly as the real listener does.
	model := "fake-4o"
	ctx := fmt.Sprintf("ctx ≈%d / 128k (%d%%)", 12000+draftRunes, (12000+draftRunes)*100/128000)
	plain := "  " + model + " · " + ctx
	if uniseg.StringWidth(plain) > w {
		plain = plain[:w]
		return sepStyle.Sprint(plain) + "\n" + markerStyle.Sprint("❯ ")
	}
	status := "  " + modelStyle.Sprint(model) + sepStyle.Sprint(" · ") + ctxStyle.Sprint(ctx)
	return sep + "\n" + status + "\n" + markerStyle.Sprint("❯ ")
}

// reserveBottomLines mirrors the real app's CJK bottom-row headroom workaround
// (macOS Terminal crashes when a CJK rune wraps on the very bottom line).
func reserveBottomLines(n int) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("\033D")
	}
	fmt.Fprintf(&b, "\033[%dA", n)
	os.Stdout.WriteString(b.String())
}

func main() {
	var rl *readline.Instance
	var err error

	var lineEmpty atomic.Bool
	lineEmpty.Store(true)
	var paused atomic.Bool

	listener := func(line []rune, pos int, key rune) ([]rune, int, bool) {
		lineEmpty.Store(len(line) == 0) // keep slashReader's empty-line flag in sync
		// Match the real app: the bottom-row CJK-wrap reserve is only needed on
		// macOS Terminal.app; skip it everywhere else so the composer sits flush
		// at the bottom with no upward bounce.
		if os.Getenv("TERM_PROGRAM") == "Apple_Terminal" {
			reserveBottomLines(2) // fixed: keep the active row 2 off the bottom
		}
		// Rebuild the live status and force a full three-line repaint on real keys
		// (also recolors a slash command as it is typed).
		if rl != nil {
			rl.SetPrompt(composerPrompt(len(line)))
		}
		if key != 0 {
			return line, pos, true
		}
		return nil, 0, false
	}

	rl, err = readline.NewEx(&readline.Config{
		Prompt:       composerPrompt(0),
		Listener:     listener,
		AutoComplete: &spikeCompleter{},
		Painter:      commandPainter,
		Stdin:        &slashReader{r: os.Stdin, empty: &lineEmpty},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	defer rl.Close()

	stop := make(chan struct{})
	if os.Getenv("NOSTREAM") == "" {
		go streamAbove(rl, stop, &paused)
	}

	out := rl.Stdout()
	fmt.Fprintln(out, sepStyle.Sprint("── type-ahead spike (slash) ──"))
	fmt.Fprintln(out, "Type '/' on an empty line → completion menu pops while stream runs above.")
	fmt.Fprintln(out, "Enter a slash command (/model /status /debug …) → modal selector, stream pauses then resumes.")
	fmt.Fprintln(out, "Plain text + Enter is discarded. Ctrl+C / Ctrl+D to quit.")

	for {
		input, err := rl.Readline()
		if err != nil { // Ctrl+C / Ctrl+D
			close(stop)
			return
		}
		lineEmpty.Store(true)
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if isSlash(input) {
			runModalCommand(input, &paused)
		}
		// plain "messages" are discarded in this spike
	}
}

// streamAbove pushes line-buffered mixed CJK/English text into rl.Stdout() at
// per-line frequency — the "does it jitter?" workload.
func streamAbove(rl *readline.Instance, stop <-chan struct{}, paused *atomic.Bool) {
	out := rl.Stdout()
	lines := []string{
		"The quick brown fox jumps over the lazy dog and keeps running.",
		"这是一段中文流式文本,用来观察输出时底部三行 composer 会不会漂移。",
		"Mixed 中英文 line: numbers 1234567890, symbols · — √ π ≈, done.",
		"更长的一行:包含标点符号、破折号——省略号……以及全角字符,ＡＢＣ。",
		"Line %d streaming — 逐行 flush 到 rl.Stdout(),看 composer 整体重画。",
		"emoji test 😀🚀🌟 and 中文 mixed with ASCII tail xyz.",
	}
	i := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		if paused.Load() { // a modal command owns the screen — hold output
			time.Sleep(50 * time.Millisecond)
			continue
		}
		line := lines[i%len(lines)]
		if strings.Contains(line, "%d") {
			line = fmt.Sprintf(line, i)
		}
		fmt.Fprintln(out, line) // always exactly one clean line + newline
		i++
		// Vary the cadence to stress both steady and bursty output.
		d := 140 * time.Millisecond
		if i%7 == 0 {
			d = 30 * time.Millisecond // occasional burst
		}
		time.Sleep(d)
	}
}
