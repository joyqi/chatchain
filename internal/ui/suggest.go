package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The composer's completion hint: one row of candidate labels with the
// highlighted one's description under it.
//
// It deliberately stays a HINT rather than becoming a menu. A menu would have
// to own keys the composer already owns — Enter above all — and every scheme
// for sharing them (arm on navigation, release on edit) puts a mode between
// the user and the most-used path in the app. Tab writes the candidate
// straight into the composer instead, so completing is one key and sending is
// still just Enter.
//
// What the row does owe the user is honesty about what it is not showing:
// candidates past the width used to be cut by a flat truncate, so cycling
// past the visible ones changed the composer while the row sat still. The
// window below follows the highlight and counts what it hides.

// suggestGap separates candidates on the row.
const suggestGap = "  "

// matchSuggestions filters the table by whole-line prefix. Entries whose
// value carries an argument ("/skills brain-page") stay hidden until the line
// has a space, so "/skills" offers the command itself rather than its whole
// catalog; once the user commits to an argument, the catalog is the point.
// Prefix matching over the WHOLE line is what keeps ordinary commands quiet
// after their arguments start — nothing registered begins with "/file some/path".
func matchSuggestions(commands []Suggestion, line string) []Suggestion {
	if !strings.HasPrefix(line, "/") || strings.Contains(line, "\n") {
		return nil
	}
	typedArg := strings.Contains(line, " ")
	var ms []Suggestion
	for _, c := range commands {
		if !typedArg && strings.Contains(c.Value, " ") {
			continue
		}
		if strings.HasPrefix(c.Value, line) {
			ms = append(ms, c)
		}
	}
	return ms
}

// suggestLabel is what a candidate contributes to the row: its explicit
// Label, else the value with its leading slash dropped.
//
// Both cases follow one rule — a candidate shows only the part the user has
// not already typed. The "/" is on screen in the composer the moment the row
// appears, and a skill entry's "/skills " prefix likewise; repeating either
// on every candidate spends the width that makes a long catalog readable.
func suggestLabel(s Suggestion) string {
	if s.Label != "" {
		return s.Label
	}
	return strings.TrimPrefix(s.Value, "/")
}

// suggestRows renders the completion hint as its two halves: the candidate
// row, which the frame places INSIDE the composer block (it belongs to the
// line being typed), and the description of the selected candidate, which
// goes below the separator in the status row's slot (it is commentary, not
// input).
//
// The description appears only once something is actually selected: under an
// unselected row it reads as if that row were chosen.
func (m *model) suggestRows() (candidates, desc string) {
	base := m.sugBase
	if base == "" {
		base = m.ta.Value()
	}
	ms := matchSuggestions(m.commands, base)
	if len(ms) == 0 {
		return "", ""
	}
	cycling := m.sugBase != "" && m.sugIdx >= 0 && m.sugIdx < len(ms)
	cur := 0
	if cycling {
		cur = m.sugIdx
	}
	candidates = m.suggestCandidates(ms, cur, cycling)
	if cycling {
		if d := ms[cur].Desc; d != "" {
			desc = "  " + faint + ansi.Truncate(d, maxInt(4, m.width-2), "…") + sgrReset
		}
	}
	return candidates, desc
}

// suggestCandidates lays the labels out on one row, windowed so the
// highlighted one is always visible and counting whatever falls outside.
// The window is computed in WHOLE candidates — half a label is not a
// candidate — and grown against the RENDERED width, because the indent, the
// "…" marker and the "+N" counter all take columns the labels cannot have.
func (m *model) suggestCandidates(ms []Suggestion, cur int, cycling bool) string {
	labels := make([]string, len(ms))
	for i, s := range ms {
		labels[i] = suggestLabel(s)
	}

	// Grow outwards from the highlight: it is the one label that must
	// survive, and expanding around it keeps its neighbours — the ones Tab
	// reaches next — in view. Each step is accepted only if the row it
	// produces still fits.
	lo, hi := cur, cur+1
	for {
		grew := false
		if hi < len(labels) && m.suggestFits(labels, lo, hi+1, cur, cycling) {
			hi, grew = hi+1, true
		}
		if lo > 0 && m.suggestFits(labels, lo-1, hi, cur, cycling) {
			lo, grew = lo-1, true
		}
		if !grew {
			break
		}
	}
	// A single label wider than the row is the one case the window cannot
	// solve; the final truncate keeps the frame intact.
	return ansi.Truncate(m.renderCandidates(labels, lo, hi, cur, cycling), maxInt(4, m.width), "…")
}

func (m *model) suggestFits(labels []string, lo, hi, cur int, cycling bool) bool {
	return ansi.StringWidth(m.renderCandidates(labels, lo, hi, cur, cycling)) <= m.width
}

// renderCandidates draws one window: the indent, a "…" when labels precede
// it, the labels themselves, and a count of everything outside.
func (m *model) renderCandidates(labels []string, lo, hi, cur int, cycling bool) string {
	var b strings.Builder
	// The tool-result marker, reused: the row is a child of the composer
	// line above it, and an indent alone left it floating.
	b.WriteString("  " + faint + "⎿ " + sgrReset)
	if lo > 0 {
		b.WriteString(faint + "… " + sgrReset)
	}
	for i := lo; i < hi; i++ {
		if i > lo {
			b.WriteString(suggestGap)
		}
		// The row is chrome — it sits under the composer and must not
		// compete with the line being typed — so candidates stay faint and
		// only the selection lifts out of it.
		if i == cur && cycling {
			b.WriteString(cyan + labels[i] + sgrReset)
		} else {
			b.WriteString(faint + labels[i] + sgrReset)
		}
	}
	// The count answers "is that all of them?" — a window alone cannot say,
	// and silently dropping candidates is what made the old row untrustworthy.
	if hidden := len(labels) - (hi - lo); hidden > 0 {
		b.WriteString(faint + fmt.Sprintf("  +%d", hidden) + sgrReset)
	}
	return b.String()
}
