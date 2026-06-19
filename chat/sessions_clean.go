package chat

import (
	"io"

	"chatchain/internal/promptui"
)

// multiSelect runs a Space-toggle / Enter-submit / Esc-cancel multi-select over
// the given rows, returning the checked indices (ok=true on submit, false on
// cancel). It uses promptui's native multi-select with its default templates,
// help line, and ESC/q cancel — so the label is just a title, with no key hints,
// templates, stdin rewriting, or prompt-restart hackery.
func multiSelect(label string, rows []string) ([]int, bool) {
	if len(rows) == 0 {
		return nil, false
	}
	size := len(rows)
	if size > 15 {
		size = 15
	}
	prompt := promptui.Select{
		Label: label,
		Items: rows,
		Size:  size,
	}
	idxs, err := prompt.RunMultiple()
	if err != nil {
		return nil, false
	}
	return idxs, true
}

// cleanSessions runs a multi-select cleanup of saved sessions, deleting the
// chosen bundles. The active session is excluded — you can't delete the one
// you're in.
func cleanSessions(w io.Writer, currentID string) {
	infos, err := ListSessions()
	if err != nil {
		ErrorStyle.Fprintf(w, "Error: %v\n", err)
		return
	}
	var sessions []SessionInfo
	for _, in := range infos {
		if in.ID != currentID {
			sessions = append(sessions, in)
		}
	}
	if len(sessions) == 0 {
		DimStyle.Fprintln(w, "No other sessions to clean.")
		return
	}

	rows := make([]string, len(sessions))
	for i, in := range sessions {
		rows[i] = sessionLabel(in)
	}

	idxs, ok := multiSelect("Clean sessions", rows)
	if !ok || len(idxs) == 0 {
		return
	}

	deleted := 0
	for _, i := range idxs {
		if derr := DeleteSession(sessions[i].ID); derr != nil {
			ErrorStyle.Fprintf(w, "Failed to delete %s: %v\n", sessions[i].ID, derr)
		} else {
			deleted++
		}
	}
	DimStyle.Fprintf(w, "Deleted %d session(s).\n", deleted)
}
