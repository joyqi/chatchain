package chat

import (
	"io"
	"strings"
	"sync/atomic"

	"github.com/ergochat/readline"
	"github.com/fatih/color"
)

// slashCommands are the interactive commands. The single source of truth shared
// by the Tab completer (candidate list), the input highlighter (commandPainter),
// and the auto-popup trigger (slashTriggerReader). Names are bare (no trailing
// space); the completer appends the space it inserts when a command is chosen.
var slashCommands = []string{
	"/file", "/files", "/clear", "/session", "/model", "/context", "/compact", "/mcp",
}

// isSlashCommand reports whether tok is exactly one of the known commands.
func isSlashCommand(tok string) bool {
	for _, c := range slashCommands {
		if c == tok {
			return true
		}
	}
	return false
}

// isSlashPrefix reports whether tok is a non-empty prefix of some known command
// (i.e. the user is partway through typing a valid command).
func isSlashPrefix(tok string) bool {
	for _, c := range slashCommands {
		if strings.HasPrefix(c, tok) {
			return true
		}
	}
	return false
}

// commandPainter is a readline Config.Painter that colorizes a leading slash
// command for real-time display: green once it is a complete known command,
// cyan while it is still a valid prefix, untouched otherwise. It only wraps the
// command token in zero-width ANSI color escapes, so the displayed width matches
// the logical buffer and the cursor stays correct — readline measures r.buf (not
// the painted output) and color-filters when measuring (see runebuf.go).
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
	tok := string(line[:end])

	var style *color.Color
	switch {
	case isSlashCommand(tok):
		style = CodeBlockStyle // green: a complete, valid command
	case isSlashPrefix(tok):
		style = CodeStyle // cyan: a valid command prefix, still typing
	default:
		return line // not a recognizable command — leave the line untouched
	}

	out := append([]rune{}, []rune(style.Sprint(tok))...)
	return append(out, line[end:]...)
}

// slashTriggerReader wraps the input stream so that typing "/" into an EMPTY
// input line injects a Tab right after it, which makes readline open (and then
// live-filter) the slash-command completion menu — matching the "type / and it
// shows up" UX. readline only *enters* completion mode on Tab, but once in it
// every further keystroke re-filters the candidates (operation.go), so one
// injected Tab is enough.
//
// Whether the line is empty is read from a flag that the Listener keeps in sync
// with readline's real buffer (slashTriggerListener), NOT inferred from the byte
// stream — so out-of-band reads like the startup cursor-position (DSR) response
// and arrow-key escape sequences cannot desync it. readline does not run the
// Listener on the submitting Enter, so the reader also resets the flag itself on
// Enter / Ctrl+C. UTF-8 multibyte runes never contain 0x2f, so CJK input cannot
// false-trigger. The injected byte can push output past the caller's buffer, so
// overflow is queued and drained next Read, mirroring escToCancelStdin.
type slashTriggerReader struct {
	r         io.Reader
	lineEmpty *atomic.Bool // shared with slashTriggerListener
	queue     []byte
}

func newSlashTriggerReader(r io.Reader, lineEmpty *atomic.Bool) *slashTriggerReader {
	return &slashTriggerReader{r: r, lineEmpty: lineEmpty}
}

func (s *slashTriggerReader) Read(p []byte) (int, error) {
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
		case b == '\r' || b == '\n' || b == 0x03: // Enter / Ctrl+C → a fresh, empty line
			s.lineEmpty.Store(true)
		case b == '/' && s.lineEmpty.Load():
			out = append(out, '\t')  // open the slash-command menu on the empty line
			s.lineEmpty.Store(false) // the "/" itself makes the line non-empty
		}
	}
	m := copy(p, out)
	if m < len(out) {
		// The injected Tab pushed output past the caller's buffer; queue the rest
		// and suppress any upstream error until it drains so the caller comes back.
		s.queue = append(s.queue, out[m:]...)
		return m, nil
	}
	return m, err
}

func (s *slashTriggerReader) Close() error {
	if c, ok := s.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// slashTriggerListener composes base (may be nil) with two hooks run after every
// keypress: it keeps lineEmpty in sync with readline's actual buffer (so the
// reader knows whether the next "/" lands on an empty line), and it forces a full
// repaint of command lines.
//
// The repaint is needed because readline's end-of-line "append" fast path
// (runebuf.go) paints only the newly typed rune, not the whole line — so after
// editing a command (e.g. delete the last char, then retype it) the new char is
// left uncolored. Returning ok=true routes the keystroke through SetWithIdx →
// full clean+print → Painter over the entire buffer, keeping the highlight
// correct. Scoped to lines starting with "/" so ordinary message typing keeps
// the cheaper fast path. If base wants to rewrite the line, that takes priority.
func slashTriggerListener(lineEmpty *atomic.Bool, base readline.Listener) readline.Listener {
	return func(line []rune, pos int, key rune) ([]rune, int, bool) {
		lineEmpty.Store(len(line) == 0)
		if base != nil {
			if nl, np, ok := base(line, pos, key); ok {
				return nl, np, ok
			}
		}
		if len(line) > 0 && line[0] == '/' {
			return line, pos, true // force a full repaint so the whole command recolors
		}
		return nil, 0, false
	}
}
