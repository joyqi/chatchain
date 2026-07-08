package chat

import (
	"io"

	"chatchain/internal/promptui"
)

// manageSessions opens a two-tab selector over saved sessions: the "Resume" tab
// single-selects a session to resume, the "Delete" tab multi-selects sessions to
// delete (the active session is excluded — you can't delete the one you're in).
// It returns the ID to resume, or "" when the user cancels or picks a delete
// action. Deletions happen here; the caller only handles the resume. The resume
// tab mirrors PickSession (still used by --resume at launch); the delete tab
// folds in the former cleanSessions logic. projectRoot scopes both tabs to the
// current project's bucket in agent mode ("" = global; see ListSessions).
func manageSessions(w io.Writer, currentID, projectRoot string) (string, error) {
	infos, err := ListSessions(projectRoot)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		DimStyle.Fprintln(w, "No sessions.")
		return "", nil
	}

	// "Resume" lists every session; "Delete" excludes the active one.
	resumeRows := make([]string, len(infos))
	for i, in := range infos {
		resumeRows[i] = sessionLabel(in)
	}
	var deletable []SessionInfo
	for _, in := range infos {
		if in.ID != currentID {
			deletable = append(deletable, in)
		}
	}
	deleteRows := make([]string, len(deletable))
	for i, in := range deletable {
		deleteRows[i] = sessionLabel(in)
	}

	resume := promptui.NewListPanel("Resume", resumeRows, false)
	resume.RuneWidth = runeWidth
	del := promptui.NewListPanel("Delete", deleteRows, true)
	del.RuneWidth = runeWidth

	tb := &promptui.Tabbed{
		Panels:    []promptui.Panel{resume, del},
		RuneWidth: runeWidth,
	}
	focused, rerr := tb.Run()
	if rerr != nil {
		return "", nil // cancelled
	}

	switch focused {
	case 0: // Resume: resume the highlighted session
		sel := resume.Selected()
		if len(sel) == 0 {
			return "", nil
		}
		return infos[sel[0]].ID, nil
	case 1: // Delete: delete the checked sessions
		idxs := del.Selected()
		deleted := 0
		for _, i := range idxs {
			if derr := DeleteSession(deletable[i].ID); derr != nil {
				ErrorStyle.Fprintf(w, "Failed to delete %s: %v\n", deletable[i].ID, derr)
			} else {
				deleted++
			}
		}
		if deleted > 0 {
			DimStyle.Fprintf(w, "Deleted %d session(s).\n", deleted)
		}
	}
	return "", nil
}
