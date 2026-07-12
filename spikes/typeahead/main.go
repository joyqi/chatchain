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
	"os"
	"strings"
	"time"

	"chatchain/internal/readline"

	"github.com/fatih/color"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

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

	listener := func(line []rune, pos int, key rune) ([]rune, int, bool) {
		// Reserve bottom headroom like the real composer.
		w := termWidth()
		lines := (len(line)*2+8)/w + 6
		if lines > 40 {
			lines = 40
		}
		reserveBottomLines(lines)
		// Rebuild the live status and force a full three-line repaint on real keys.
		if rl != nil {
			rl.SetPrompt(composerPrompt(len(line)))
		}
		if key != 0 {
			return line, pos, true
		}
		return nil, 0, false
	}

	rl, err = readline.NewEx(&readline.Config{
		Prompt:   composerPrompt(0),
		Listener: listener,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	defer rl.Close()

	stop := make(chan struct{})
	go streamAbove(rl, stop)

	out := rl.Stdout()
	fmt.Fprintln(out, sepStyle.Sprint("── type-ahead spike ──"))
	fmt.Fprintln(out, "Type (incl. Chinese) while lines stream above. Watch the ❯ line, status, and CJK for jitter.")
	fmt.Fprintln(out, "Enter to 'send' (input is discarded, loop continues). Ctrl+C / Ctrl+D to quit.")

	for {
		if _, err := rl.Readline(); err != nil { // Ctrl+C / Ctrl+D
			close(stop)
			return
		}
	}
}

// streamAbove pushes line-buffered mixed CJK/English text into rl.Stdout() at
// per-line frequency — the "does it jitter?" workload.
func streamAbove(rl *readline.Instance, stop <-chan struct{}) {
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
		fmt.Fprintf(out, lines[i%len(lines)]+"\n", i)
		i++
		// Vary the cadence to stress both steady and bursty output.
		d := 140 * time.Millisecond
		if i%7 == 0 {
			d = 30 * time.Millisecond // occasional burst
		}
		time.Sleep(d)
	}
}
