package chat

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/manifoldco/promptui"
)

type multiSelectAction int32

const (
	actionToggle multiSelectAction = iota
	actionSubmit
	actionCancel
)

// multiSelectStdin adapts a single-select promptui into a multi-select: promptui
// only ends a Run on Enter and won't bind a custom toggle key, so this stdin
// wrapper rewrites keys before promptui sees them — Space and Enter both end the
// Run (so the loop can act), and the action they meant is recorded for the caller
// to read after Run returns:
//
//	Space      → Enter, action = toggle  (flip the highlighted row, keep going)
//	Enter      → Enter, action = submit  (commit: delete the checked rows)
//	Esc/Ctrl+C → escCancelSeq, action = cancel  (clean exit via the success path)
//
// Arrow/SS3 escape sequences pass through so navigation still works. Overflow
// from an expansion is queued, mirroring escToCancelStdin.
type multiSelectStdin struct {
	r      io.Reader
	action atomic.Int32
	queue  []byte
}

func (m *multiSelectStdin) Read(p []byte) (int, error) {
	if len(m.queue) > 0 {
		n := copy(p, m.queue)
		m.queue = m.queue[n:]
		return n, nil
	}
	n, err := m.r.Read(p)
	if n == 0 {
		return n, err
	}
	var out []byte
	for i := 0; i < n; i++ {
		b := p[i]
		loneEsc := b == 0x1b && !(i+1 < n && (p[i+1] == '[' || p[i+1] == 'O'))
		switch {
		case b == ' ':
			m.action.Store(int32(actionToggle))
			out = append(out, '\r')
		case b == '\r' || b == '\n':
			m.action.Store(int32(actionSubmit))
			out = append(out, '\r')
		case b == 0x03 || loneEsc:
			m.action.Store(int32(actionCancel))
			out = append(out, escCancelSeq...)
		default:
			out = append(out, b)
		}
	}
	k := copy(p, out)
	if k < len(out) {
		m.queue = append(m.queue, out[k:]...)
		return k, nil
	}
	return k, err
}

func (m *multiSelectStdin) Close() error {
	if c, ok := m.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// multiSelect runs a Space-toggle / Enter-submit / Esc-cancel multi-select over
// the row labels (each rendered with a leading checkbox). Returns the checked
// indices with ok=true on submit, or (nil, false) on cancel. promptui has no
// native multi-select; multiSelectStdin turns Space into a run-ending key and the
// loop re-renders with the cursor preserved so it feels like in-place toggling.
func multiSelect(label string, rows []string) ([]int, bool) {
	if len(rows) == 0 {
		return nil, false
	}
	stdin := &multiSelectStdin{r: os.Stdin}
	checked := make([]bool, len(rows))
	cursor := 0

	for {
		items := make([]string, len(rows))
		for i, r := range rows {
			mark := " "
			if checked[i] {
				mark = "x"
			}
			items[i] = fmt.Sprintf("[%s] %s", mark, r)
		}

		size := len(items)
		if size > 15 {
			size = 15
		}
		// Derive the top-of-window fresh from the cursor (centered): promptui
		// doesn't expose the scroll offset it ended at, so a carried value would
		// go stale and snap the view to an earlier page.
		scroll := cursor - size/2
		if max := len(items) - size; scroll > max {
			scroll = max
		}
		if scroll < 0 {
			scroll = 0
		}

		prompt := promptui.Select{
			Label:        label,
			Items:        items,
			Size:         size,
			Stdin:        stdin,
			HideHelp:     true,
			HideSelected: true,
			Templates: &promptui.SelectTemplates{
				Active:   `▸ {{ . | cyan }}`,
				Inactive: `  {{ . }}`,
			},
		}
		idx, _, rerr := prompt.RunCursorAt(cursor, scroll)
		if rerr != nil {
			return nil, false
		}
		switch multiSelectAction(stdin.action.Load()) {
		case actionCancel:
			return nil, false
		case actionSubmit:
			var out []int
			for i, on := range checked {
				if on {
					out = append(out, i)
				}
			}
			return out, true
		case actionToggle:
			checked[idx] = !checked[idx]
			cursor = idx
		}
	}
}

// cleanSessions runs the multi-select cleanup of saved sessions, deleting the
// chosen bundles. The active session is excluded — you can't delete the one
// you're in.
func cleanSessions(w io.Writer, currentID string) {
	infos, err := ListSessions()
	if err != nil {
		ErrorStyle.Fprintf(w, "Error: %v\n", err)
		return
	}
	var list []SessionInfo
	for _, in := range infos {
		if in.ID != currentID {
			list = append(list, in)
		}
	}
	if len(list) == 0 {
		DimStyle.Fprintln(w, "No other sessions to clean.")
		return
	}
	rows := make([]string, len(list))
	for i, in := range list {
		rows[i] = sessionLabel(in)
	}
	idxs, ok := multiSelect("Clean sessions — Space toggles · Enter deletes · Esc cancels", rows)
	if !ok || len(idxs) == 0 {
		return
	}
	deleted := 0
	for _, i := range idxs {
		if derr := DeleteSession(list[i].ID); derr != nil {
			ErrorStyle.Fprintf(w, "Failed to delete %s: %v\n", list[i].ID, derr)
		} else {
			deleted++
		}
	}
	DimStyle.Fprintf(w, "Deleted %d session(s).\n", deleted)
}
