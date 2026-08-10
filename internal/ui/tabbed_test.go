package ui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	"github.com/charmbracelet/x/ansi"
)

// newTestInput is a focused real-cursor field, styled like the panels build
// them (no prompt, no internal SGR).
func newTestInput(value string, cursor int) *textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetVirtualCursor(false)
	ti.SetStyles(textinput.Styles{})
	ti.SetValue(value)
	ti.Focus()
	ti.SetCursor(cursor)
	return &ti
}

// A value that fits leaves the window alone: no pan, cursor column == the
// value's display width up to the cursor.
func TestInputFieldFits(t *testing.T) {
	const boxW = 20
	ti := newTestInput("short", 5)
	off := 0
	view, cur := inputField(ti, &off, boxW)
	if off != 0 {
		t.Fatalf("off = %d, want 0 (value fits)", off)
	}
	if cur != 5 {
		t.Fatalf("cursor col = %d, want 5", cur)
	}
	// textinput draws a blank cell where the cursor sits, so the rendered
	// field runs one column past the value.
	if got := stripSGR(view); got != "short " {
		t.Fatalf("view = %q, want %q", got, "short ")
	}
}

// The regression this file exists for: with a value longer than the box, the
// cursor must stay INSIDE the box. The field's own scrolling used to leave it
// at the value's absolute column — far to the right of the field, or past the
// end of the row.
func TestInputFieldLongValueKeepsCursorInsideBox(t *testing.T) {
	const boxW = 10
	value := strings.Repeat("abcdefghij", 5) // 50 columns
	ti := newTestInput(value, len([]rune(value)))
	off := 0
	view, cur := inputField(ti, &off, boxW)

	if cur < 0 || cur > boxW-1 {
		t.Fatalf("cursor col = %d, want within [0,%d]", cur, boxW-1)
	}
	if w := ansi.StringWidth(view); w > boxW {
		t.Fatalf("view is %d columns wide, want at most %d", w, boxW)
	}
	// The window shows the tail of the value plus the cursor's own cell.
	if got, want := stripSGR(view), value[len(value)-(boxW-1):]+" "; got != want {
		t.Fatalf("view = %q, want the tail %q", got, want)
	}
}

// Panning is two-way: walking the cursor back to the start scrolls the window
// home again (the old code could only ever look right).
func TestInputFieldPansBothWays(t *testing.T) {
	const boxW = 10
	value := strings.Repeat("abcdefghij", 5)
	ti := newTestInput(value, len([]rune(value)))
	off := 0
	inputField(ti, &off, boxW)
	if off == 0 {
		t.Fatal("window never panned right on a long value")
	}

	ti.SetCursor(0)
	view, cur := inputField(ti, &off, boxW)
	if off != 0 || cur != 0 {
		t.Fatalf("off/cur = %d/%d, want 0/0 after the cursor returned home", off, cur)
	}
	if got := stripSGR(view); got != value[:boxW] {
		t.Fatalf("view = %q, want the head %q", got, value[:boxW])
	}

	// Mid-value: the cursor stays visible and the window shows what surrounds it.
	ti.SetCursor(25)
	_, cur = inputField(ti, &off, boxW)
	if cur < 0 || cur > boxW-1 {
		t.Fatalf("mid-value cursor col = %d, want within [0,%d]", cur, boxW-1)
	}
	if off+cur != 25 {
		t.Fatalf("window start %d + cursor col %d != absolute column 25", off, cur)
	}
}

// Wide (CJK) runes count as two columns on both sides of the arithmetic: the
// window start and the cursor column.
func TestInputFieldWideRunes(t *testing.T) {
	const boxW = 10
	value := strings.Repeat("宽字", 8) // 32 columns, 16 runes
	ti := newTestInput(value, 16)
	off := 0
	view, cur := inputField(ti, &off, boxW)

	if w := ansi.StringWidth(view); w > boxW {
		t.Fatalf("view is %d columns wide, want at most %d", w, boxW)
	}
	if cur > boxW-1 {
		t.Fatalf("cursor col = %d, want within the box", cur)
	}
	if off+cur != 32 {
		t.Fatalf("window start %d + cursor col %d != the value's 32 columns", off, cur)
	}
}

// The window never pans past the end: a value that shrinks (backspace) must
// not leave blank columns inside the box while text sits off to the left.
func TestInputFieldNeverPansPastTheEnd(t *testing.T) {
	const boxW = 10
	ti := newTestInput(strings.Repeat("x", 40), 40)
	off := 0
	inputField(ti, &off, boxW)

	ti.SetValue(strings.Repeat("x", 12))
	ti.SetCursor(12)
	view, cur := inputField(ti, &off, boxW)
	if want := 12 + 1 - boxW; off != want {
		t.Fatalf("off = %d, want %d (pinned to the shortened value's end)", off, want)
	}
	if got := ansi.StringWidth(view); got != boxW {
		t.Fatalf("view is %d columns, want a full box of %d", got, boxW)
	}
	if off+cur != 12 {
		t.Fatalf("window start %d + cursor col %d != 12", off, cur)
	}
}
