package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"chatchain/internal/textwidth"
)

// step drives one Update and returns the (mutated) model.
func step(t *testing.T, m *model, msg tea.Msg) *model {
	t.Helper()
	nm, _ := m.Update(msg)
	return nm.(*model)
}

func newTestModel(t *testing.T) *model {
	t.Helper()
	var w atomic.Int64
	m := newModel(&w, nil)
	return step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
}

func typeText(t *testing.T, m *model, s string) *model {
	t.Helper()
	for _, r := range s {
		m = step(t, m, tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}
	return m
}

func enter(t *testing.T, m *model) *model {
	t.Helper()
	return step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
}

func ctrlC(t *testing.T, m *model) *model {
	t.Helper()
	return step(t, m, tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
}

func content(m *model) string { return m.View().Content }

// openList opens a one-panel list surface (the Select shape).
func openList(t *testing.T, m *model, title string, items []string, reply chan TabbedResult) *model {
	t.Helper()
	return step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{Title: title, Kind: PanelList, Items: items}}}, reply: reply})
}

// TestSubmitDeliversToWaiter: with a pending ReadInput, Enter delivers the
// input directly (no queueing).
func TestSubmitDeliversToWaiter(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = typeText(t, m, "hello")
	m = enter(t, m)
	select {
	case r := <-reply:
		if r.err != nil || r.in.Text != "hello" {
			t.Fatalf("delivered %+v", r)
		}
	default:
		t.Fatal("waiter not delivered")
	}
	if m.waiter != nil {
		t.Fatal("waiter not cleared")
	}
}

// TestQueueThenDrain: submits with no waiter queue (visible as » rows above
// the separator); the next readReq drains the queue in order.
func TestQueueThenDrain(t *testing.T) {
	m := newTestModel(t)
	m = typeText(t, m, "first")
	m = enter(t, m)
	m = typeText(t, m, "second")
	m = enter(t, m)
	if len(m.queue) != 2 {
		t.Fatalf("queue = %v", m.queue)
	}
	if c := content(m); !strings.Contains(c, "» first") || !strings.Contains(c, "» second") {
		t.Fatalf("queue rows missing:\n%s", c)
	}
	// Queue renders above the separator (content side).
	c := content(m)
	if strings.Index(c, "» first") > strings.Index(c, "───") {
		t.Fatal("queue not above the separator")
	}

	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	if r := <-reply; r.in.Text != "first" {
		t.Fatalf("drained %q, want first", r.in.Text)
	}
	if len(m.queue) != 1 || m.queue[0] != "second" {
		t.Fatalf("queue after drain = %v", m.queue)
	}
}

// TestSelectBelowComposer: the selector renders BELOW the composer line and
// arrow+Enter resolve the reply; the composer cursor is hidden meanwhile.
func TestSelectBelowComposer(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openList(t, m, "/model", []string{"a", "b", "c"}, reply)

	c := content(m)
	if strings.Index(c, "❯") > strings.Index(c, "/model") {
		t.Fatalf("selector not below the composer:\n%s", c)
	}
	if v := m.View(); v.Cursor != nil {
		t.Fatal("real cursor should hide while a surface is open")
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = enter(t, m)
	if r := <-reply; r.Cancelled || r.Panels[0].Cursor != 1 {
		t.Fatalf("select result %+v", r)
	}
	if m.surf != nil {
		t.Fatal("surface not closed")
	}
}

// TestSelectEscCancels: ESC cancels the surface without firing turn scopes.
func TestSelectEscCancels(t *testing.T) {
	m := newTestModel(t)
	var cancelled atomic.Bool
	m = step(t, m, scopePushMsg{cancel: func() { cancelled.Store(true) }})
	reply := make(chan TabbedResult, 1)
	m = openList(t, m, "t", []string{"x"}, reply)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if r := <-reply; !r.Cancelled {
		t.Fatal("esc should cancel the select")
	}
	if cancelled.Load() {
		t.Fatal("surface ESC must not fire the turn cancel scope")
	}
}

// TestInterruptRestoresQueueToDraft: Ctrl+C with an active scope fires the
// turn cancel and folds queued submits (plus the half-typed draft) into a
// multi-line composer draft.
func TestInterruptRestoresQueueToDraft(t *testing.T) {
	m := newTestModel(t)
	fired := false
	m = step(t, m, scopePushMsg{cancel: func() { fired = true }})
	m = typeText(t, m, "queued-A")
	m = enter(t, m)
	m = typeText(t, m, "queued-B")
	m = enter(t, m)
	m = typeText(t, m, "half")

	m = ctrlC(t, m)
	if !fired {
		t.Fatal("turn cancel not fired")
	}
	if len(m.queue) != 0 || len(m.cancels) != 0 {
		t.Fatalf("queue/cancels not cleared: %v %d", m.queue, len(m.cancels))
	}
	want := "queued-A\nqueued-B\nhalf"
	if got := m.ta.Value(); got != want {
		t.Fatalf("draft = %q, want %q", got, want)
	}
	if m.ta.Height() != 3 {
		t.Fatalf("draft height = %d, want 3", m.ta.Height())
	}
}

// TestIdleCtrlCInterrupts: with no scopes, Ctrl+C surfaces ErrInterrupted to
// the pending ReadInput.
func TestIdleCtrlCInterrupts(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = ctrlC(t, m)
	if r := <-reply; r.err != ErrInterrupted {
		t.Fatalf("err = %v, want ErrInterrupted", r.err)
	}
	_ = m
}

// TestEscCancelsInnermostScope: ESC fires the top (tool) scope, leaving the
// turn scope in place.
func TestEscCancelsInnermostScope(t *testing.T) {
	m := newTestModel(t)
	var turn, tool bool
	m = step(t, m, scopePushMsg{cancel: func() { turn = true }})
	m = step(t, m, scopePushMsg{cancel: func() { tool = true }})
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if !tool || turn {
		t.Fatalf("esc fired turn=%v tool=%v, want tool only", turn, tool)
	}
	if len(m.cancels) != 1 {
		t.Fatalf("cancels = %d, want 1 (turn kept)", len(m.cancels))
	}
}

// TestRegionMorph pins the staging-window contract (the answer to "why can't
// the preview be overwritten in place?" — it is): preview growth STEALS tail
// rows (commits, never shrinks); the block's rendered lines REPLACE the
// preview in place; total height never exceeds tailKeep and never shrinks
// across the flush.
func TestRegionMorph(t *testing.T) {
	var overflows [][]string
	var snaps []regionMsg
	r := &region{emit: func(over []string, snap regionMsg) {
		if len(over) > 0 {
			overflows = append(overflows, append([]string{}, over...))
		}
		snaps = append(snaps, snap)
	}}
	rows := func(s regionMsg) int {
		n := len(s.tail)
		if s.label != "" {
			n += 1 + len(s.ptail)
		}
		return n
	}

	// Warm the window with committed lines.
	r.commit([]string{"p1", "p2", "p3", "p4"})
	if got := rows(snaps[len(snaps)-1]); got != tailKeep {
		t.Fatalf("warm rows = %d, want %d", got, tailKeep)
	}

	// Preview opens and grows: tail rows are stolen (committed), height stays.
	r.openPreview("rendering table…")
	for _, l := range []string{"|a|", "|b|", "|c|", "|d|"} {
		r.previewLine(l)
	}
	last := snaps[len(snaps)-1]
	if got := rows(last); got != tailKeep {
		t.Fatalf("rows during preview = %d, want constant %d", got, tailKeep)
	}
	if len(last.ptail) != previewWindow || last.ptail[0] != "|b|" {
		t.Fatalf("preview tail = %v, want last %d source lines", last.ptail, previewWindow)
	}
	var flat []string
	for _, o := range overflows {
		flat = append(flat, o...)
	}
	if strings.Join(flat, ",") != "p1,p2,p3,p4" {
		t.Fatalf("stolen tail commits = %v, want p1..p4 in order", flat)
	}

	// Deferred close + the rendered block: replaced IN PLACE, height constant,
	// head rows overflow above.
	r.closePreview()
	overflows = nil
	r.commit([]string{"t1", "t2", "t3", "t4", "t5", "t6"})
	last = snaps[len(snaps)-1]
	if last.label != "" {
		t.Fatal("preview not replaced by the block")
	}
	if strings.Join(last.tail, ",") != "t3,t4,t5,t6" {
		t.Fatalf("window after morph = %v, want t3..t6", last.tail)
	}
	if len(overflows) != 1 || strings.Join(overflows[0], ",") != "t1,t2" {
		t.Fatalf("overflow = %v, want [t1 t2]", overflows)
	}
	if got := rows(last); got != tailKeep {
		t.Fatalf("rows after morph = %d, want %d (no shrink across flush)", got, tailKeep)
	}
}

// TestRegionMorphResidue: a preview collapsing into FEWER lines than it
// occupied (the thinking window folding to its one-line marker) must not
// shrink the window — the uncovered rows stay as residue and later commits
// consume them top-down.
func TestRegionMorphResidue(t *testing.T) {
	var snaps []regionMsg
	r := &region{emit: func(over []string, snap regionMsg) { snaps = append(snaps, snap) }}
	rows := func(s regionMsg) int {
		n := len(s.tail) + len(s.residue)
		if s.label != "" {
			n += 1 + len(s.ptail)
		}
		return n
	}
	last := func() regionMsg { return snaps[len(snaps)-1] }

	// Warm to full height, then a thinking preview takes the window over.
	r.commit([]string{"p1", "p2", "p3", "p4"})
	r.openPreview("rendering table…")
	for _, l := range []string{"think1", "think2", "think3"} {
		r.previewLine(l)
	}
	if got := rows(last()); got != tailKeep {
		t.Fatalf("rows during thinking = %d, want %d", got, tailKeep)
	}

	// Collapse: one marker line replaces the header; the three thinking rows
	// become residue — height unchanged, composer must not move.
	r.closePreview()
	r.commit([]string{"◇ thought for 3s"})
	s := last()
	if s.label != "" || len(s.residue) != 3 || s.residue[0] != "think1" {
		t.Fatalf("after collapse: label=%q residue=%v, want 3 residue rows", s.label, s.residue)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after collapse = %d, want %d (the composer would move)", got, tailKeep)
	}

	// Later commits overwrite residue top-down, height still constant.
	r.commit([]string{"", "reply-1"})
	s = last()
	if len(s.residue) != 1 || s.residue[0] != "think3" {
		t.Fatalf("residue after 2 lines = %v, want [think3]", s.residue)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows mid-consumption = %d, want %d", got, tailKeep)
	}
	r.commit([]string{"reply-2", "reply-3"})
	s = last()
	if len(s.residue) != 0 {
		t.Fatalf("residue should be fully consumed, got %v", s.residue)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after consumption = %d, want %d", got, tailKeep)
	}

	// A new preview's header row also consumes a residue row.
	r.openPreview("rendering table…")
	r.previewLine("t1")
	r.previewLine("t2")
	r.closePreview()
	r.commit([]string{"◇ thought for 1s"}) // residue = [t1 t2]
	before := rows(last())
	r.openPreview("rendering code…")
	s = last()
	if len(s.residue) != 1 || rows(s) != before {
		t.Fatalf("openPreview over residue: residue=%v rows=%d, want 1 residue row and %d rows", s.residue, rows(s), before)
	}

	// dropPreview (turn boundary) clears residue too.
	r.dropPreview()
	if s = last(); len(s.residue) != 0 || s.label != "" {
		t.Fatalf("dropPreview left residue=%v label=%q", s.residue, s.label)
	}
}

// TestRegionCallPreview: the tool-call lifecycle widget — header + live
// status row — keeps its clock across relabels, and settling (deferred close
// + the header/result commits) never changes the window height.
func TestRegionCallPreview(t *testing.T) {
	var snaps []regionMsg
	r := &region{emit: func(over []string, snap regionMsg) { snaps = append(snaps, snap) }}
	rows := func(s regionMsg) int {
		n := len(s.tail) + len(s.residue)
		if s.label != "" {
			n += 1 + len(s.ptail)
			if !s.since.IsZero() {
				n++
			}
		}
		return n
	}
	last := func() regionMsg { return snaps[len(snaps)-1] }

	r.commit([]string{"p1", "p2", "p3", "p4"})
	r.openCallPreview("[write_file …]")
	s := last()
	if s.since.IsZero() || s.label != "[write_file …]" {
		t.Fatalf("call preview not raised: %+v", s)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows with call widget = %d, want %d", got, tailKeep)
	}
	started := s.since

	// The live detail lands on the status row without touching the height.
	r.setCallDetail("0.4k tokens")
	s = last()
	if s.detail != "0.4k tokens" {
		t.Fatalf("detail not recorded: %+v", s)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after detail = %d, want %d", got, tailKeep)
	}

	// Expanding the header (composing → full args) keeps the clock and detail.
	r.openCallPreview("[write_file path:a.txt]")
	s = last()
	if s.label != "[write_file path:a.txt]" || !s.since.Equal(started) {
		t.Fatalf("ensure should relabel in place keeping the clock: %+v", s)
	}
	if s.detail != "0.4k tokens" {
		t.Fatalf("ensure should keep the detail: %+v", s)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after relabel = %d, want %d", got, tailKeep)
	}

	// Settle: deferred close + the single header commit takes the spinner
	// row; the status row leaves a blank placeholder consumed by the result.
	r.closePreview()
	r.setCallDetail("late") // after the deferred close: no-op
	r.commit([]string{"[write_file path:a.txt]"})
	s = last()
	if s.label != "" || !s.since.IsZero() || s.detail != "" {
		t.Fatalf("widget should be settled with detail cleared: %+v", s)
	}
	if len(s.residue) != 1 || s.residue[0] != "" {
		t.Fatalf("status row should leave a blank placeholder, got residue=%q", s.residue)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after settle = %d, want %d (the composer would move)", got, tailKeep)
	}
	r.commit([]string{"  ⎿ wrote 1.2 KB"})
	s = last()
	if len(s.residue) != 0 {
		t.Fatalf("result line should consume the placeholder, got %q", s.residue)
	}
	if got := rows(s); got != tailKeep {
		t.Fatalf("rows after result = %d, want %d", got, tailKeep)
	}
}

// TestCallPreviewRendering: the widget renders a spinner header over the live
// "⎿ elapsed" row, with the cancel hint while a cancel scope is active.
func TestCallPreviewRendering(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, regionMsg{label: "[bash …]", since: time.Now().Add(-3 * time.Second)})
	got := stripSGR(content(m))
	if !strings.Contains(got, "[bash …]") {
		t.Fatalf("widget header missing:\n%s", got)
	}
	if !strings.Contains(got, "⎿ 3s") {
		t.Fatalf("elapsed status row missing:\n%s", got)
	}
	if strings.Contains(got, "ESC to cancel") {
		t.Fatalf("cancel hint without a cancel scope:\n%s", got)
	}

	m = step(t, m, scopePushMsg{cancel: func() {}})
	if got := stripSGR(content(m)); !strings.Contains(got, "⎿ 3s · ESC to cancel") {
		t.Fatalf("cancel hint missing with an active scope:\n%s", got)
	}

	// The live detail rides the status row ahead of the elapsed time.
	m = step(t, m, regionMsg{label: "[bash …]", since: time.Now().Add(-3 * time.Second), detail: "1.2k tokens"})
	if got := stripSGR(content(m)); !strings.Contains(got, "⎿ 1.2k tokens · 3s · ESC to cancel") {
		t.Fatalf("detail missing from the status row:\n%s", got)
	}
}

// TestRegionPreviewOverPreview: opening a preview over an existing one folds
// the old rows into residue (back-to-back streamed tool calls) — height flat.
func TestRegionPreviewOverPreview(t *testing.T) {
	var snaps []regionMsg
	r := &region{emit: func(over []string, snap regionMsg) { snaps = append(snaps, snap) }}
	rows := func(s regionMsg) int {
		n := len(s.tail) + len(s.residue)
		if s.label != "" {
			n += 1 + len(s.ptail)
		}
		return n
	}

	r.commit([]string{"p1", "p2", "p3", "p4"})
	r.openPreview("Calling write_file")
	for _, l := range []string{"a1", "a2", "a3"} {
		r.previewLine(l)
	}
	before := rows(snaps[len(snaps)-1])

	r.closePreview()
	r.openPreview("Calling bash")
	s := snaps[len(snaps)-1]
	if s.label != "Calling bash" || len(s.ptail) != 0 {
		t.Fatalf("new preview wrong: label=%q ptail=%v", s.label, s.ptail)
	}
	if len(s.residue) != 3 || s.residue[0] != "a1" {
		t.Fatalf("old preview rows should fold into residue, got %v", s.residue)
	}
	if got := rows(s); got != before {
		t.Fatalf("rows across preview swap = %d, want %d", got, before)
	}
}

// TestRegionRendering: the model renders the staging window above the
// separator — tail as-is, preview dim under a spinner header.
func TestRegionRendering(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, regionMsg{tail: []string{"line-A"}, label: "rendering table…", ptail: []string{"|src|"}})
	c := content(m)
	for _, want := range []string{"line-A", "rendering table…", "|src|"} {
		if !strings.Contains(c, want) {
			t.Fatalf("missing %q:\n%s", want, c)
		}
	}
	if strings.Index(c, "line-A") > strings.Index(c, "rendering table…") {
		t.Fatal("tail must render above the preview")
	}
	if strings.Index(c, "rendering table…") > strings.Index(c, "───") {
		t.Fatalf("staging window not above the separator:\n%s", c)
	}
}

// TestRegionSnapshotChangesView: a commit's region snapshot must change the
// rendered view (the renderer skips flush on identical views, which would
// strand the real cursor after insertAbove). The staged tail rotation IS the
// view change — no artificial glyph needed.
func TestRegionSnapshotChangesView(t *testing.T) {
	m := newTestModel(t)
	before := content(m)
	m = step(t, m, regionMsg{tail: []string{"committed line"}})
	after := content(m)
	if after == before {
		t.Fatal("view unchanged after insert — cursor restore would be skipped")
	}
	// The next commit rotates the tail → changes the view again.
	m = step(t, m, regionMsg{tail: []string{"committed line", "next"}})
	if content(m) == after {
		t.Fatal("second insert did not change the view")
	}
	// And the idle status line carries no spinner glyph.
	if strings.ContainsAny(stripSGR(content(m)), "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Fatalf("idle frame should have no spinner glyph:\n%s", content(m))
	}
}

// TestStatusLine: fields render; narrow widths truncate to a single row.
func TestStatusLine(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, statusMsg(StatusData{Model: "gpt-4o", CtxUsed: 12000, CtxWindow: 128000, Estimated: true}))
	c := content(m)
	for _, want := range []string{"gpt-4o", "≈12k", "128k", "(9%)"} {
		if !strings.Contains(c, want) {
			t.Fatalf("status missing %q:\n%s", want, c)
		}
	}
	m = step(t, m, tea.WindowSizeMsg{Width: 20, Height: 24})
	for _, line := range strings.Split(stripSGR(content(m)), "\n") {
		if w := len([]rune(line)); w > 20+1 { // rune count ≈ upper bound here
			t.Fatalf("line overflows narrow width: %q", line)
		}
	}
}

// TestViewerScrolls: a viewer taller than its window scrolls with ↓ and closes
// on q, replying to the facade.
func TestViewerScrolls(t *testing.T) {
	m := newTestModel(t)
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strings.Repeat("x", 3) + string(rune('A'+i%26))
	}
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{Title: "/status", Kind: PanelView, Lines: lines, Height: 5}}}, reply: reply})
	if !strings.Contains(content(m), lines[0]) || strings.Contains(content(m), lines[6]) {
		t.Fatalf("viewer window wrong:\n%s", content(m))
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if !strings.Contains(content(m), lines[5]) {
		t.Fatal("viewer did not scroll")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"}))
	select {
	case <-reply:
	default:
		t.Fatal("viewer close not replied")
	}
}

// TestReadCancelRevokesWaiter: a ReadInput whose ctx died must revoke its
// waiter so a later submit doesn't go to a dead channel.
func TestReadCancelRevokesWaiter(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = step(t, m, readCancelMsg{reply: reply})
	m = typeText(t, m, "late")
	m = enter(t, m)
	if len(m.queue) != 1 {
		t.Fatalf("late submit should queue, queue = %v", m.queue)
	}
	_ = context.Background()
}

// stripSGR removes SGR escapes for width assertions.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestRegionBlankLineSurvivesOverflow: markdown's block spacing is a lone ""
// line; when it overflows the window by itself it must still land in
// scrollback (bubbletea's insertAbove drops empty strings — the region
// substitutes a single space).
func TestRegionBlankLineSurvivesOverflow(t *testing.T) {
	var overflows []string
	r := &region{emit: func(over []string, snap regionMsg) {
		overflows = append(overflows, over...)
	}}
	// Fill the window, then push a blank through it alone.
	r.commit([]string{"a", "b", "c", "d"})
	r.commit([]string{""}) // blank enters the window, "a" overflows
	for _, l := range []string{"e", "f", "g", "h"} {
		r.commit([]string{l}) // b, c, d overflow; then the blank ALONE
	}
	want := []string{"a", "b", "c", "d", ""}
	if len(overflows) != len(want) {
		t.Fatalf("overflows = %q, want %q", overflows, want)
	}
	for i, w := range want {
		if overflows[i] != w {
			t.Fatalf("overflow[%d] = %q, want %q (full: %q)", i, overflows[i], w, overflows)
		}
	}
	// And the Println payload for a lone blank must not be the empty string
	// (insertAbove would drop it) — a space stands in.
	if got := joinOverflow([]string{""}); got != " " {
		t.Fatalf("joinOverflow lone blank = %q, want a space", got)
	}
	if got := joinOverflow([]string{"", "x"}); got != "\nx" {
		t.Fatalf("joinOverflow mixed = %q", got)
	}
}

// TestHistoryNavigation: ↑ recalls submitted inputs, ↓ walks back to the
// saved draft; queued submits enter history too.
func TestHistoryNavigation(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = typeText(t, m, "one")
	m = enter(t, m)
	<-reply
	m = typeText(t, m, "two")
	m = enter(t, m) // no waiter → queued (history too)

	m = typeText(t, m, "dra")
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if got := m.ta.Value(); got != "two" {
		t.Fatalf("↑ = %q, want two", got)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if got := m.ta.Value(); got != "one" {
		t.Fatalf("↑↑ = %q, want one", got)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if got := m.ta.Value(); got != "dra" {
		t.Fatalf("↓↓ = %q, want the saved draft", got)
	}
}

// TestSlashTabCompletion: a "/" prefix shows suggestions; Tab cycles the
// matches of the prefix captured at the first press.
func TestSlashTabCompletion(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, setCommandsMsg([]string{"/file", "/session", "/status", "/model"}))
	m = typeText(t, m, "/s")
	if c := content(m); !strings.Contains(c, "/session") || !strings.Contains(c, "/status") {
		t.Fatalf("suggestion row missing:\n%s", c)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if got := m.ta.Value(); got != "/session" {
		t.Fatalf("tab = %q, want /session", got)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if got := m.ta.Value(); got != "/status" {
		t.Fatalf("tab tab = %q, want /status (cycling the ORIGINAL prefix)", got)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if got := m.ta.Value(); got != "/session" {
		t.Fatalf("cycle wrap = %q, want /session", got)
	}
}

// TestPasteTags: a multi-line paste collapses to a [#N …] tag in the
// composer (a thousand-line blob is unusable to edit), while BOTH sides of
// the submitted input carry real content — Text in full for the model,
// Display expanded for the transcript echo.
func TestPasteTags(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = step(t, m, tea.PasteMsg{Content: "line1\nline2\nline3"})
	if v := m.ta.Value(); !strings.HasPrefix(v, "[#1 line1… 3 lines]") {
		t.Fatalf("composer = %q, want a paste tag", v)
	}
	m = enter(t, m)
	r := <-reply
	if r.in.Text != "line1\nline2\nline3" {
		t.Fatalf("Text = %q, want expanded paste", r.in.Text)
	}
	if r.in.Display != "line1\nline2\nline3" {
		t.Fatalf("Display = %q, want the echo expanded", r.in.Display)
	}
	// Single-line pastes insert verbatim.
	m = step(t, m, tea.PasteMsg{Content: "inline"})
	if got := m.ta.Value(); got != "inline" {
		t.Fatalf("single-line paste = %q", got)
	}
}

// A long paste echoes bounded — head plus a count — so one paste cannot bury
// the reply, while the model still receives every line.
func TestPasteEchoTruncates(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	content := strings.TrimRight(b.String(), "\n")

	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = step(t, m, tea.PasteMsg{Content: content})
	m = typeText(t, m, " please review")
	m = enter(t, m)
	r := <-reply

	if !strings.Contains(r.in.Text, "line60") {
		t.Fatal("Text must carry the whole paste")
	}
	echo := strings.Split(r.in.Display, "\n")
	if len(echo) != pasteEchoMaxLines+1 {
		t.Fatalf("echo rows = %d, want %d head rows + the count", len(echo), pasteEchoMaxLines+1)
	}
	if echo[pasteEchoMaxLines] != "… +40 more lines please review" {
		t.Fatalf("last echo row = %q", echo[pasteEchoMaxLines])
	}
	if strings.Contains(r.in.Display, "line60") {
		t.Fatal("echo must stop at the cap")
	}
}

// ↑ recall restores the composer's own text — the TAG, not the blob — and a
// re-submit expands it again from the store, so the model never receives a
// literal placeholder.
func TestPasteHistoryRecallKeepsTag(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan inputResult, 1)
	m = step(t, m, readReqMsg{reply: reply})
	m = step(t, m, tea.PasteMsg{Content: "alpha\nbeta"})
	m = enter(t, m)
	<-reply

	m = step(t, m, readReqMsg{reply: reply})
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if v := m.ta.Value(); !strings.HasPrefix(v, "[#1") {
		t.Fatalf("recalled composer = %q, want the tag (an editable stand-in)", v)
	}
	m = enter(t, m)
	if r := <-reply; r.in.Text != "alpha\nbeta" {
		t.Fatalf("re-submitted Text = %q, want the paste expanded again", r.in.Text)
	}
}

// TestTabbedCommitAll: Tab switches panels; Enter commits EVERY panel's state
// (the /model questionnaire semantics) — list cursor, multi checks, slider.
func TestTabbedCommitAll(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelList, Items: []string{"a", "b"}},
		{Title: "Flags", Kind: PanelMulti, Items: []string{"x", "y", "z"}},
		{Title: "Temp", Kind: PanelSlider, Min: 0, Max: 2, Step: 0.1},
	}}, reply: reply})

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})) // Model → b
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))  // → Flags
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})) // cursor y
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Text: " "}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))   // → Temp
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})) // default → 0.0
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})) // 0.1
	m = enter(t, m)

	r := <-reply
	if r.Cancelled || r.Focused != 2 {
		t.Fatalf("result %+v", r)
	}
	if r.Panels[0].Cursor != 1 {
		t.Fatalf("list cursor = %d, want 1", r.Panels[0].Cursor)
	}
	if len(r.Panels[1].Checked) != 1 || r.Panels[1].Checked[0] != 1 {
		t.Fatalf("multi checked = %v, want [1]", r.Panels[1].Checked)
	}
	if r.Panels[2].Value == nil || *r.Panels[2].Value != 0.1 {
		t.Fatalf("slider = %v, want 0.1", r.Panels[2].Value)
	}
}

// TestSliderDefaultTransitions: below Min falls back to default; g resets.
func TestSliderDefaultTransitions(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "T", Kind: PanelSlider, Min: 0, Max: 1, Step: 0.5},
	}}, reply: reply})
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})) // default → 0
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))  // 0 → default
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'G', Text: "G"}))
	m = enter(t, m)
	r := <-reply
	if r.Panels[0].Value == nil || *r.Panels[0].Value != 1 {
		t.Fatalf("slider = %v, want Max after G", r.Panels[0].Value)
	}
}

// TestBrowserDescendAndChoose: Enter on a directory descends (no commit);
// Enter on a file commits with the chosen path.
func TestBrowserDescendAndChoose(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "pick.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Add", Kind: PanelBrowser, Dir: root},
	}}, reply: reply})

	// entries: ../, sub/ — move to sub/ and descend.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = enter(t, m)
	if m.surf == nil {
		t.Fatal("descend must not commit")
	}
	// entries now: ../, pick.txt — choose the file.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = enter(t, m)
	r := <-reply
	if r.Cancelled || r.Panels[0].Path != filepath.Join(sub, "pick.txt") {
		t.Fatalf("chosen = %+v", r)
	}
}

// TestSurfaceLiveRefresh: panels with Refresh update their items on the tick.
func TestSurfaceLiveRefresh(t *testing.T) {
	m := newTestModel(t)
	n := 0
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{
		RefreshEvery: 100,
		Panels: []Panel{{Title: "Live", Kind: PanelView, Refresh: func() []string {
			n++
			return []string{fmt.Sprintf("tick %d", n)}
		}}},
	}, reply: reply})
	m = step(t, m, surfTickMsg{gen: m.surfGen})
	if !strings.Contains(content(m), "tick 1") {
		t.Fatalf("refresh not applied:\n%s", content(m))
	}
	m = step(t, m, surfTickMsg{gen: m.surfGen})
	if !strings.Contains(content(m), "tick 2") {
		t.Fatal("second refresh not applied")
	}
	// A stale generation must not refresh.
	before := n
	m = step(t, m, surfTickMsg{gen: m.surfGen - 1})
	if n != before {
		t.Fatal("stale tick refreshed the surface")
	}
}

// TestViewKeysV1Parity: G jumps to the bottom (offset, not cursor), ←→ and
// Ctrl+B/F page, Space pages forward, h/l pan a non-wrap view, and "c" on a
// non-View panel must NOT cancel the surface (regression).
func TestViewKeysV1Parity(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("row-%02d with some very long tail content %d", i, i)
	}
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{Title: "v", Kind: PanelView, Lines: lines, Height: 5}}}, reply: reply})

	// G → bottom.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'G', Text: "G", Mod: tea.ModShift}))
	if c := content(m); !strings.Contains(c, "row-39") {
		t.Fatalf("G did not reach the bottom:\n%s", c)
	}
	// g → top.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'g', Text: "g"}))
	if c := content(m); !strings.Contains(c, "row-00") {
		t.Fatal("g did not return to the top")
	}
	// → pages forward; Ctrl+B pages back.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if c := content(m); !strings.Contains(c, "row-05") || strings.Contains(c, "row-00") {
		t.Fatalf("→ did not page forward:\n%s", c)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'b', Mod: tea.ModCtrl}))
	if c := content(m); !strings.Contains(c, "row-00") {
		t.Fatal("Ctrl+B did not page back")
	}
	// Space pages forward (v1 view semantics).
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Text: " "}))
	if c := content(m); !strings.Contains(c, "row-05") {
		t.Fatal("Space did not page forward")
	}
	// h/l pan a non-wrap view horizontally.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'l', Text: "l"}))
	if c := content(m); strings.Contains(c, "row-05 ") && !strings.Contains(c, "ow-05") {
		t.Fatalf("l did not pan right:\n%s", c)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"}))
	<-reply

	// Regression: "c" on a List panel must not cancel.
	reply2 := make(chan TabbedResult, 1)
	m = openList(t, m, "t", []string{"x", "y"}, reply2)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'c', Text: "c"}))
	if m.surf == nil {
		t.Fatal("'c' on a list cancelled the surface")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply2
}

// TestListPagingParity: ←→ page a list by its visible height.
func TestListPagingParity(t *testing.T) {
	items := make([]string, 30)
	for i := range items {
		items[i] = fmt.Sprintf("item-%02d", i)
	}
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{Title: "l", Kind: PanelList, Items: items, Height: 6}}}, reply: reply})
	_ = content(m) // render once to record the page size
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	m = enter(t, m)
	if r := <-reply; r.Panels[0].Cursor != 6 {
		t.Fatalf("cursor after → = %d, want 6 (one page)", r.Panels[0].Cursor)
	}
}

// TestBusyInStatusLine: the busy indicator lives on the (permanent) status
// row — toggling it must not change the frame's line count, which was the
// last remaining composer bounce.
func TestBusyInStatusLine(t *testing.T) {
	m := newTestModel(t)
	// Give the status row context figures — the ctx segment hides without
	// them (token-less providers), and this test locates the row by it.
	m = step(t, m, statusMsg(StatusData{Model: "gpt-4o", CtxUsed: 1000, CtxWindow: 128000}))
	before := content(m)
	rowsBefore := strings.Count(before, "\n")

	m = step(t, m, busyOnMsg{label: "Compacting context…"})
	during := content(m)
	if strings.Count(during, "\n") != rowsBefore {
		t.Fatalf("busy ON changed the frame height:\n%s", during)
	}
	if !strings.Contains(during, "Compacting context…") {
		t.Fatalf("busy label not in the status line:\n%s", during)
	}
	// The label sits on the SAME row as the model/ctx status.
	for _, line := range strings.Split(stripSGR(during), "\n") {
		if strings.Contains(line, "Compacting context…") && !strings.Contains(line, "ctx") {
			t.Fatalf("busy not on the status row: %q", line)
		}
	}

	m = step(t, m, busyOffMsg{})
	after := content(m)
	if strings.Count(after, "\n") != rowsBefore {
		t.Fatal("busy OFF changed the frame height")
	}
	if strings.Contains(after, "Compacting context…") {
		t.Fatal("busy label not cleared")
	}
}

// TestBusyDetail: the live sub-state renders after the label without touching
// the phase clock, resets with a new phase, and is a no-op when idle.
func TestBusyDetail(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, busyOnMsg{label: "Composing tool call — write_file"})
	since := m.busy.since
	m = step(t, m, busyDetailMsg("4.2 KB"))
	if got := stripSGR(content(m)); !strings.Contains(got, "Composing tool call — write_file · 4.2 KB") {
		t.Fatalf("detail not rendered:\n%s", got)
	}
	if !m.busy.since.Equal(since) {
		t.Fatal("detail update reset the phase clock")
	}

	// A new phase clears the previous detail.
	m = step(t, m, busyOnMsg{label: "Compacting context…"})
	if got := stripSGR(content(m)); strings.Contains(got, "4.2 KB") {
		t.Fatalf("stale detail survived a phase change:\n%s", got)
	}

	// Detail with no busy phase is dropped, not resurrected later.
	m = step(t, m, busyOffMsg{})
	m = step(t, m, busyDetailMsg("ghost"))
	if got := stripSGR(content(m)); strings.Contains(got, "ghost") {
		t.Fatalf("detail rendered with no busy phase:\n%s", got)
	}
}

// TestWrappedComposerLayout pins the user-confirmed frame layout: the
// composer sits between TWO separators; the status line lives below the
// bottom one (frame bottom); an open surface or the slash-suggestion row
// replaces the status row (surface > suggestions > status); the queue stays
// above the TOP separator.
func TestWrappedComposerLayout(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, statusMsg(StatusData{Model: "gpt-4o", CtxUsed: 1, CtxWindow: 100}))
	m = typeText(t, m, "queued")
	m = enter(t, m) // no waiter → queue row above the top separator

	c := stripSGR(content(m))
	lines := strings.Split(c, "\n")
	var sepIdx []int
	composerIdx, statusIdx, queueIdx := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "───"):
			sepIdx = append(sepIdx, i)
		case strings.Contains(l, "❯"):
			composerIdx = i
		case strings.Contains(l, "gpt-4o · ctx"):
			statusIdx = i
		case strings.Contains(l, "» queued"):
			queueIdx = i
		}
	}
	if len(sepIdx) != 2 {
		t.Fatalf("want exactly 2 separators, got %d:\n%s", len(sepIdx), c)
	}
	if !(queueIdx < sepIdx[0] && sepIdx[0] < composerIdx && composerIdx < sepIdx[1] && sepIdx[1] < statusIdx) {
		t.Fatalf("layout order wrong (queue=%d sep=%v composer=%d status=%d):\n%s",
			queueIdx, sepIdx, composerIdx, statusIdx, c)
	}
	if statusIdx != len(lines)-1 {
		t.Fatalf("status not the frame's last row:\n%s", c)
	}

	// A surface replaces the status row.
	reply := make(chan TabbedResult, 1)
	m = openList(t, m, "/model", []string{"a"}, reply)
	c = stripSGR(content(m))
	if strings.Contains(c, "gpt-4o · ctx") {
		t.Fatalf("status visible while a surface is open:\n%s", c)
	}
	if strings.Index(c, "❯") > strings.Index(c, "/model") {
		t.Fatal("surface not below the composer")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply
	if !strings.Contains(stripSGR(content(m)), "gpt-4o · ctx") {
		t.Fatal("status not restored after the surface closed")
	}

	// The slash-suggestion row replaces the status row too.
	m = step(t, m, setCommandsMsg([]string{"/model"}))
	m = typeText(t, m, "/m")
	c = stripSGR(content(m))
	if strings.Contains(c, "gpt-4o · ctx") || !strings.Contains(c, "/model") {
		t.Fatalf("suggestions should replace the status row:\n%s", c)
	}
}

// TestResizeFlushesStagingTail: a width change must flush the staged tail
// into scrollback (reflow ghosts duplicate the frame's top rows — content —
// once per resize event; an empty tail shrinks the ghost surface to the
// separator). Height-only changes don't reflow and must not flush.
func TestResizeFlushesStagingTail(t *testing.T) {
	var overflows []string
	r := &region{emit: func(over []string, snap regionMsg) {
		overflows = append(overflows, over...)
	}}
	r.commit([]string{"a", "b"})
	r.openPreview("rendering…")

	var w atomic.Int64
	m := newModel(&w, nil)
	m.flushTail = r.flushTail
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Width change → the returned cmd flushes the tail (preview survives).
	nm, cmd := m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = nm.(*model)
	if cmd == nil {
		t.Fatal("width change did not schedule a tail flush")
	}
	cmd()
	if strings.Join(overflows, ",") != "a,b" {
		t.Fatalf("tail not flushed on resize: %q", overflows)
	}
	r.mu.Lock()
	if len(r.tail) != 0 || r.label == "" {
		t.Fatalf("tail=%v label=%q — want empty tail, preview kept", r.tail, r.label)
	}
	r.mu.Unlock()

	// Height-only change → no flush cmd.
	_, cmd = m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	if cmd != nil {
		t.Fatal("height-only change scheduled a flush")
	}
}

// TestCursorRowHighlight: v1 semantics — a plain cursor row's text renders
// cyan; a row carrying its own ANSI keeps it (marker only); the tab bar keeps
// identical padding across focus states so switching never changes its width.
func TestCursorRowHighlight(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "A", Kind: PanelList, Items: []string{"plain-row", "\x1b[32mstyled\x1b[0m-row"}},
		{Title: "B", Kind: PanelView, Lines: []string{"x"}},
	}}, reply: reply})

	c := content(m)
	if !strings.Contains(c, cyan+"plain-row"+sgrReset) {
		t.Fatalf("plain cursor row not highlighted:\n%q", c)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if c := content(m); strings.Contains(c, cyan+"\x1b[32mstyled") {
		t.Fatalf("styled row must keep its own colors, marker only:\n%q", c)
	}

	// Tab bar width is focus-invariant.
	barWidth := func(s string) int {
		for _, l := range strings.Split(stripSGR(s), "\n") {
			if strings.Contains(l, " A ") || strings.Contains(l, " B ") {
				return len([]rune(l))
			}
		}
		return -1
	}
	w1 := barWidth(content(m))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	w2 := barWidth(content(m))
	if w1 != w2 || w1 <= 0 {
		t.Fatalf("tab bar width changed on focus switch: %d → %d", w1, w2)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply
}

// TestSliderProgressBarAndChipTitle pins the tabbed-surface restyle: a
// single-panel surface titles with the focused-chip style (no faint dashes),
// and the slider renders a native progress bar whose origin column does not
// shift between the "default" and numeric-value states.
func TestSliderProgressBarAndChipTitle(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Temperature", Kind: PanelSlider, Min: 0, Max: 2, Step: 0.1},
	}}, reply: reply})

	c := content(m)
	if strings.Contains(c, "── Temperature") {
		t.Fatalf("single-panel title still uses faint dashes:\n%q", c)
	}
	if !strings.Contains(c, revOn+" Temperature "+sgrReset) {
		t.Fatalf("single-panel title missing focused-chip style:\n%q", c)
	}

	barCol := func(s string) int {
		for _, l := range strings.Split(stripSGR(s), "\n") {
			if i := strings.IndexAny(l, "█▌░"); i >= 0 {
				return i
			}
		}
		return -1
	}
	col0 := barCol(c)
	if col0 < 0 {
		t.Fatalf("slider progress bar not rendered:\n%q", c)
	}
	if !strings.Contains(stripSGR(c), "default") {
		t.Fatalf("default state label missing:\n%q", c)
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})) // default → min
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})) // step up
	c = content(m)
	if !strings.ContainsAny(c, "█▌") {
		t.Fatalf("bar has no filled cells after stepping up:\n%q", c)
	}
	if got := barCol(c); got != col0 {
		t.Fatalf("bar origin shifted between default and value states: %d → %d", col0, got)
	}
	plain := stripSGR(c)
	barRow := -1
	rows := strings.Split(plain, "\n")
	for i, l := range rows {
		if strings.ContainsAny(l, "█▌░") {
			barRow = i
		}
	}
	if barRow < 1 || strings.TrimSpace(rows[barRow-1]) != "" || strings.TrimSpace(rows[barRow+1]) != "" {
		t.Fatalf("bar row not padded by blank rows:\n%q", plain)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply
}

// TestWrapANSICarriesStyle pins the styled-wrap contract: rows produced by
// wrapANSI are self-contained — each continuation row re-opens the SGR state
// its line had at the break, so a viewport clipped mid-line keeps the styling
// (the /tools MCP tab bug: scrolling past a wrapped faint line's first row
// dropped the faint on the rest).
func TestWrapANSICarriesStyle(t *testing.T) {
	rows := wrapANSI("\x1b[2mfaint methods list that wraps across rows\x1b[0m", 16)
	if len(rows) < 3 {
		t.Fatalf("expected ≥3 rows, got %d: %q", len(rows), rows)
	}
	for i, r := range rows[1:] {
		if !strings.HasPrefix(r, "\x1b[2m") {
			t.Fatalf("continuation row %d lost the faint state: %q", i+1, r)
		}
	}

	// Reset mid-line stops the carry; combined params ("0;7") clear-then-set.
	rows = wrapANSI("\x1b[7mrev\x1b[0m plain tail that wraps onward", 12)
	for i, r := range rows[1:] {
		if strings.Contains(r, "\x1b[7m") {
			t.Fatalf("row %d re-opened style past its reset: %q", i+1, r)
		}
	}
	if got := sgrCarry("", "\x1b[0;7mx"); got != "\x1b[7m" {
		t.Fatalf("combined reset+set carry = %q, want \\x1b[7m", got)
	}
	if got := sgrCarry("\x1b[2m", "a\x1b[36mb"); got != "\x1b[2;36m" {
		t.Fatalf("merged carry = %q, want \\x1b[2;36m", got)
	}

	// CJK-aware: no row exceeds the column budget.
	for _, r := range wrapANSI("\x1b[2m方法 tools/list tools/call 中文继续中文继续\x1b[0m", 10) {
		if w := textwidth.StringWidth(stripSGR(r)); w > 10 {
			t.Fatalf("row overflows budget: %d cols %q", w, r)
		}
	}
}

// TestViewScrollKeepsWrappedStyle drives the real surface: a Wrap view whose
// faint line wraps over several rows, scrolled so only continuation rows are
// visible — they must still carry the faint SGR.
func TestViewScrollKeepsWrappedStyle(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	long := "\x1b[2m" + strings.Repeat("methods word ", 30) + "\x1b[0m"
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "MCP", Kind: PanelView, Wrap: true, Height: 3, Lines: []string{long, "tail"}},
	}}, reply: reply})

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	c := content(m)
	var body []string
	for _, l := range strings.Split(c, "\n") {
		if strings.Contains(l, "methods word") {
			body = append(body, l)
		}
	}
	if len(body) == 0 {
		t.Fatalf("no wrapped rows visible:\n%q", c)
	}
	for _, l := range body {
		if !strings.Contains(l, "\x1b[2m") {
			t.Fatalf("visible continuation row lost faint:\n%q", l)
		}
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply
}

// TestStatusLineFieldHues pins the v1 composer-status color contract (ported
// from chat/inputchrome_test.go, died with the old stack): the model and ctx
// segments carry DIFFERENT hues (cyan vs green, both faint) so the fields
// read as distinct at a glance, with faint separators between them.
func TestStatusLineFieldHues(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, statusMsg(StatusData{Model: "gpt-4o", CtxUsed: 1000, CtxWindow: 128000, Estimated: true}))
	line := m.statusLine()
	if !strings.Contains(line, cyan+faint+"gpt-4o"+sgrReset) {
		t.Fatalf("model segment not cyan+faint:\n%q", line)
	}
	if !strings.Contains(line, green+faint+"ctx ≈1k / 128k (0%)"+sgrReset) {
		t.Fatalf("ctx segment not green+faint (or format drifted):\n%q", line)
	}

	// No model yet: an em-dash placeholder keeps the field visible.
	m = step(t, m, statusMsg(StatusData{}))
	if !strings.Contains(stripSGR(m.statusLine()), "—") {
		t.Fatalf("missing em-dash placeholder:\n%q", m.statusLine())
	}
}

// TestChunkOverflowBelowScreenHeight pins the insertAbove safety contract: a
// scrollback insert taller than the screen desyncs the renderer's frame anchor
// (InsertLine is clamped to the screen height while the scroll is not), so the
// region must never Println a block of ≥ screen-height lines. Repro'd via a
// large /session resume echo eating the composer separators and status line.
func TestChunkOverflowBelowScreenHeight(t *testing.T) {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i)
	}
	chunks := chunkOverflow(lines, 30)
	total := 0
	for _, c := range chunks {
		if len(c) > 15 { // h/2
			t.Fatalf("chunk of %d lines exceeds half the screen height", len(c))
		}
		total += len(c)
	}
	if total != 100 {
		t.Fatalf("chunking lost lines: %d/100", total)
	}
	if got := chunks[0][0]; got != "l0" {
		t.Fatalf("order broken: first line %q", got)
	}
	if got := chunks[len(chunks)-1][len(chunks[len(chunks)-1])-1]; got != "l99" {
		t.Fatalf("order broken: last line %q", got)
	}
	if chunkOverflow(nil, 30) != nil {
		t.Fatal("empty overflow must produce no chunks")
	}
	// Tiny terminals still make progress.
	if c := chunkOverflow(lines[:5], 1); len(c) != 3 {
		t.Fatalf("h=1 chunking = %d chunks, want 3 (size 2)", len(c))
	}
}

// typeInto sends each rune as a key press.
func typeInto(t *testing.T, m *model, s string) *model {
	t.Helper()
	for _, r := range s {
		m = step(t, m, tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}
	return m
}

// TestInputPanel pins the PanelInput contract: letters (including the q/g/j/k
// navigation keys of other panels) TYPE into the field; the value survives a
// Tab round-trip; Enter commits the text into PanelResult.Text; the rendered
// row carries the subtle background and never exceeds the box width (content
// beyond it scrolls horizontally); pastes are flattened to one line; the REAL
// cursor is placed inside the field.
func TestInputPanel(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelInput, Placeholder: "model name", InputWidth: 10},
		{Title: "Other", Kind: PanelList, Items: []string{"a", "b"}},
	}}, reply: reply})

	// Letter keys type — q must not cancel, g/j/k must not navigate.
	m = typeInto(t, m, "qgjk")
	if m.surf == nil {
		t.Fatal("q cancelled the surface while typing into an input")
	}
	if got := m.surf.ps[0].input.Value(); got != "qgjk" {
		t.Fatalf("typed value = %q, want qgjk", got)
	}

	// The real cursor is inside the field (composer's cursor is suppressed).
	if v := m.View(); v.Cursor == nil || v.Cursor.Y == 0 {
		t.Fatalf("real cursor not placed in the input field: %+v", v.Cursor)
	}

	// Paste flattens to a single line.
	m = step(t, m, tea.PasteMsg{Content: "-multi\nline"})
	if got := m.surf.ps[0].input.Value(); got != "qgjk-multi line" {
		t.Fatalf("paste value = %q", got)
	}

	// Tab away and back keeps the value; list keys navigate again meanwhile.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'j', Text: "j"}))
	if m.surf.ps[1].cursor != 1 {
		t.Fatal("j did not navigate the list after tabbing away from the input")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if got := m.surf.ps[0].input.Value(); got != "qgjk-multi line" {
		t.Fatalf("value lost across tab round-trip: %q", got)
	}

	// Long content scrolls horizontally: the box row stays bounded.
	m = typeInto(t, m, "-0123456789abcdef")
	for _, l := range strings.Split(stripSGR(content(m)), "\n") {
		if strings.Contains(l, "def") && textwidth.StringWidth(l) > 10+6 {
			t.Fatalf("input row wider than the box: %q", l)
		}
	}
	if !strings.Contains(content(m), inputBg()) {
		t.Fatal("input row lost its background styling")
	}

	// Enter commits ALL tabs; the text lands in PanelResult.Text.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	r := <-reply
	if r.Cancelled || r.Panels[0].Text != "qgjk-multi line-0123456789abcdef" {
		t.Fatalf("commit result = %+v", r)
	}
	if r.Panels[1].Cursor != 1 {
		t.Fatal("list tab state not committed alongside the input")
	}
}

// TestInputPanelEscCancels: Esc (and Ctrl+C) still cancel while typing.
func TestInputPanelEscCancels(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelInput},
	}}, reply: reply})
	m = typeInto(t, m, "abc")
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if r := <-reply; !r.Cancelled {
		t.Fatal("Esc did not cancel the input surface")
	}
}

// TestInputPanelColors pins the field's color contract: the box is an
// adaptive background shade (per detected terminal tone), typed text renders
// in the DEFAULT foreground (no reverse video, no fg recolor), and the
// placeholder is faint on the same background.
func TestInputPanelColors(t *testing.T) {
	t.Cleanup(func() { SetDarkBackground(true) })

	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelInput, Placeholder: "hint", InputWidth: 12},
	}}, reply: reply})

	// Placeholder: faint, on the dark-side shade by default.
	c := content(m)
	if !strings.Contains(c, "\x1b[48;5;236m") {
		t.Fatalf("dark-side background shade missing:\n%q", c)
	}
	if !strings.Contains(c, faint+"hint") {
		t.Fatalf("placeholder not faint:\n%q", c)
	}

	// Typed text: bare runes right after the background+pad — no reverse, no
	// fg color, no faint.
	m = typeInto(t, m, "abc")
	c = content(m)
	if !strings.Contains(c, "\x1b[48;5;236m abc") {
		t.Fatalf("typed text not default-foreground on the shade:\n%q", c)
	}
	if strings.Contains(c, revOn+" abc") || strings.Contains(c, faint+"abc") {
		t.Fatalf("typed text carries reverse/faint styling:\n%q", c)
	}

	// Light backgrounds flip to the light-side shade.
	SetDarkBackground(false)
	if c := content(m); !strings.Contains(c, "\x1b[48;5;254m") {
		t.Fatalf("light-side shade missing:\n%q", c)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	<-reply
}

// EnterAdvances (the ask wizard shape): Enter on a non-last tab moves to the
// NEXT tab instead of committing — unvisited questions must not be silently
// submitted with defaults; only the last tab's Enter commits all.
func TestTabbedEnterAdvances(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{EnterAdvances: true, Panels: []Panel{
		{Title: "Q1", Kind: PanelList, Items: []string{"a", "b"}},
		{Title: "Q2", Kind: PanelInput},
	}}, reply: reply})

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})) // pick "b"
	m = enter(t, m)                                             // NOT last tab: advance, no commit
	select {
	case <-reply:
		t.Fatal("Enter on a non-last tab must not commit")
	default:
	}
	if m.surf == nil || m.surf.focus != 1 {
		t.Fatalf("focus after advance = %+v", m.surf)
	}

	m = typeText(t, m, "custom")
	m = enter(t, m) // last tab: commits all
	r := <-reply
	if r.Cancelled || r.Panels[0].Cursor != 1 || r.Panels[1].Text != "custom" {
		t.Fatalf("result = %+v", r)
	}
}

// The inline Custom editor: Enter on the "Other…" row opens it IN PLACE;
// ESC closes just the editor (the ask survives); Enter with text proceeds —
// a single-select advances the wizard with the custom answer, a Multi checks
// the row and stays for more toggles.
func TestTabbedInlineCustom(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{EnterAdvances: true, Panels: []Panel{
		{Title: "Q1", Kind: PanelList, Items: []string{"a", "b"}, Custom: true},
		{Title: "Q2", Kind: PanelMulti, Items: []string{"x", "y"}, Custom: true},
	}}, reply: reply})

	// Q1: cursor to "Other…" (index 2), Enter opens the editor.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = enter(t, m)
	if !m.surf.ps[0].editing {
		t.Fatal("Enter on Other must open the inline editor")
	}
	// ESC closes JUST the editor.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.surf == nil || m.surf.ps[0].editing {
		t.Fatal("ESC in the editor must return to the options, not decline the ask")
	}
	// Reopen, type, Enter → wizard advances to Q2 with the custom answer.
	m = enter(t, m)
	m = typeText(t, m, "zig")
	m = enter(t, m)
	select {
	case <-reply:
		t.Fatal("custom confirm on a non-last tab must advance, not commit")
	default:
	}
	if m.surf.focus != 1 {
		t.Fatalf("focus = %d, want Q2", m.surf.focus)
	}

	// Q2 (multi): Space on "Other…" opens the editor; Enter checks and stays.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace}))
	if !m.surf.ps[1].editing {
		t.Fatal("Space on an unchecked Other must open the editor")
	}
	m = typeText(t, m, "bird")
	m = enter(t, m)
	if m.surf == nil {
		t.Fatal("multi editor confirm must stay on the panel")
	}
	if !m.surf.ps[1].checked[2] {
		t.Fatal("confirmed custom must check the Other row")
	}
	// Enter on the Other row WITH text behaves like a normal row: commit
	// (last tab) instead of reopening the editor — "type, Enter, Enter" is
	// the natural submit flow. Check "x" first via Space elsewhere.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = enter(t, m) // cursor back on Other (has text): commits, no reopen
	r := <-reply
	if r.Cancelled {
		t.Fatal("commit expected")
	}
	if r.Panels[0].Cursor != 2 || r.Panels[0].Custom != "zig" {
		t.Fatalf("Q1 result = %+v", r.Panels[0])
	}
	if r.Panels[1].Custom != "bird" || len(r.Panels[1].Checked) != 2 {
		t.Fatalf("Q2 result = %+v", r.Panels[1])
	}
}

// The field cursor's X is a RUNE index; wide (CJK) runes must convert to
// display columns or the real terminal cursor drifts one column left per
// wide rune ("乒乓球" = 3 runes, 6 columns).
func TestInputCursorColsCJK(t *testing.T) {
	ti := textinput.New()
	ti.SetValue("乒乓球")
	if got := inputCursorCols(ti, 3); got != 6 {
		t.Fatalf("CJK cols = %d, want 6", got)
	}
	ti.SetValue("a中b")
	if got := inputCursorCols(ti, 2); got != 3 {
		t.Fatalf("mixed cols = %d, want 3 (a=1 + 中=2)", got)
	}
	if got := inputCursorCols(ti, 99); got != 4 {
		t.Fatalf("clamped cols = %d, want 4", got)
	}
}

// A PanelSwitch drives one boolean: Space toggles, ←→ set the state
// directly, Enter commits it — and both states render the exact same row
// geometry (the knob slides, the width stays).
func TestTabbedSwitch(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Image", Kind: PanelSwitch},
	}}, reply: reply})

	toggleRow := func() string {
		for _, l := range strings.Split(content(m), "\n") {
			if strings.Contains(l, "●") {
				return strings.TrimRight(stripSGRText(l), " ")
			}
		}
		t.Fatal("no switch row rendered")
		return ""
	}
	offRow := toggleRow()
	if !strings.Contains(offRow, "Off") {
		t.Fatalf("initial row = %q, want the Off state", offRow)
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace}))
	if !m.surf.ps[0].on {
		t.Fatal("Space must toggle the switch on")
	}
	onRow := toggleRow()
	if !strings.Contains(onRow, "On") {
		t.Fatalf("on row = %q, want the On state", onRow)
	}
	if textwidth.StringWidth(offRow) != textwidth.StringWidth(onRow) {
		t.Fatalf("state flip changed the row geometry:\n%q\n%q", offRow, onRow)
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	if m.surf.ps[0].on {
		t.Fatal("← must set the switch off")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if !m.surf.ps[0].on {
		t.Fatal("→ must set the switch on")
	}

	m = enter(t, m)
	r := <-reply
	if r.Cancelled || !r.Panels[0].On {
		t.Fatalf("commit = %+v, want On=true", r)
	}
}

// A provider without token accounting pushes StatusData with zero context
// figures — the ctx segment must vanish, not read "ctx 0 / 0".
func TestStatusLineHidesCtxWithoutTokens(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, statusMsg(StatusData{Model: "seedream-5.0-pro"}))
	line := stripSGR(content(m))
	if !strings.Contains(line, "seedream-5.0-pro") {
		t.Fatalf("model missing from status:\n%s", line)
	}
	if strings.Contains(line, "ctx") {
		t.Fatalf("token-less status must hide the ctx segment:\n%s", line)
	}
}

// PanelPicker: preview pane beside the list. The preview renderer is called
// once per (selection, geometry) — not once per frame — the cursor row is
// truncated to the list column, and the current item's detail line follows
// the cursor.
func TestTabbedPicker(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	reply := make(chan TabbedResult, 1)

	calls := 0
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{
		Title:   "Pick",
		Kind:    PanelPicker,
		Items:   []string{"first", "second " + strings.Repeat("x", 200)},
		Details: []string{"detail-one", "detail-two"},
		Preview: func(index, maxCols, maxRows int) []string {
			calls++
			if maxCols <= 0 || maxRows <= 0 {
				t.Errorf("preview geometry = %dx%d", maxCols, maxRows)
			}
			// SGR-laden rows, like imgterm's: the pad must be computed on
			// DISPLAY width or the list column drifts.
			return []string{"\x1b[38;5;42m" + fmt.Sprintf("PREVIEW-%d", index) + "\x1b[0m"}
		},
	}}}, reply: reply})

	view := content(m)
	if !strings.Contains(view, "PREVIEW-0") || !strings.Contains(stripSGR(view), "first") {
		t.Fatalf("preview and list must render side by side:\n%s", view)
	}
	if !strings.Contains(view, "detail-one") {
		t.Fatalf("detail line missing:\n%s", view)
	}
	// Re-rendering the same frame must not re-invoke the preview.
	before := calls
	_ = content(m)
	if calls != before {
		t.Fatalf("preview re-rendered without a change (%d → %d)", before, calls)
	}

	// Every row stays within the terminal width (padding is display-width
	// based, so the list column can't drift).
	for _, line := range strings.Split(stripSGR(view), "\n") {
		if w := textwidth.StringWidth(line); w > 100 {
			t.Fatalf("row exceeds terminal width (%d): %q", w, line)
		}
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	view = content(m)
	if !strings.Contains(view, "PREVIEW-1") || !strings.Contains(view, "detail-two") {
		t.Fatalf("moving the cursor must refresh preview and detail:\n%s", view)
	}
	if calls == before {
		t.Fatal("selection change must re-render the preview")
	}

	m = enter(t, m)
	if r := <-reply; r.Cancelled || r.Panels[0].Cursor != 1 {
		t.Fatalf("commit = %+v, want cursor 1", r)
	}
}

// A terminal too narrow for two columns drops the preview entirely rather
// than squeezing both into an unreadable width.
func TestTabbedPickerNarrowFallback(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, tea.WindowSizeMsg{Width: 50, Height: 30})
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{
		Title: "Pick", Kind: PanelPicker, Items: []string{"only"},
		Preview: func(int, int, int) []string { return []string{"PREVIEW"} },
	}}}, reply: reply})
	if view := content(m); strings.Contains(view, "PREVIEW") {
		t.Fatalf("narrow terminal must drop the preview:\n%s", view)
	}
	if view := stripSGR(content(m)); !strings.Contains(view, "only") {
		t.Fatalf("list must still render:\n%s", view)
	}
}

// The list column starts at the SAME screen column on every row, including
// rows whose preview cell is dense with SGR (ANSI-aware padding).
func TestTabbedPickerColumnAlignment(t *testing.T) {
	m := newTestModel(t)
	m = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{{
		Title: "Pick", Kind: PanelPicker,
		Items: []string{"alpha", "beta", "gamma"},
		Preview: func(index, maxCols, maxRows int) []string {
			row := "\x1b[48;2;10;20;30m\x1b[38;2;40;50;60m" + strings.Repeat("▀", 12) + "\x1b[0m"
			rows := make([]string, maxRows)
			for i := range rows {
				rows[i] = row
			}
			return rows
		},
	}}}, reply: reply})

	var cols []int
	for _, line := range strings.Split(stripSGR(content(m)), "\n") {
		if i := strings.IndexAny(line, "▸"); i >= 0 && strings.Contains(line, "alpha") {
			cols = append(cols, textwidth.StringWidth(line[:i]))
		}
		for _, item := range []string{"beta", "gamma"} {
			if i := strings.Index(line, item); i >= 0 {
				cols = append(cols, textwidth.StringWidth(line[:i-2]))
			}
		}
	}
	if len(cols) != 3 {
		t.Fatalf("expected 3 list rows, measured %v", cols)
	}
	for _, c := range cols[1:] {
		if c != cols[0] {
			t.Fatalf("list column drifts across rows: %v", cols)
		}
	}
}

// TestTakeQueuedMessages: the steering drain pops the contiguous PREFIX of
// plain messages — a slash command stops the take (it must not be reordered
// against the messages queued behind it), and an empty or command-headed
// queue takes nothing.
func TestTakeQueuedMessages(t *testing.T) {
	m := newTestModel(t)
	for _, s := range []string{"first", "second", "/model", "third"} {
		m = typeText(t, m, s)
		m = enter(t, m)
	}

	reply := make(chan []Input, 1)
	m = step(t, m, takeQueuedMsg{reply: reply})
	taken := <-reply
	if len(taken) != 2 || taken[0].Text != "first" || taken[1].Text != "second" {
		t.Fatalf("taken = %+v, want [first second]", taken)
	}
	if len(m.queue) != 2 || m.queue[0] != "/model" || m.queue[1] != "third" {
		t.Fatalf("queue after take = %v, want [/model third]", m.queue)
	}
	if c := content(m); strings.Contains(c, "first") || !strings.Contains(c, "/model") {
		t.Fatalf("queue rows must reflect the take:\n%s", c)
	}

	// Command at the head: nothing to take.
	reply = make(chan []Input, 1)
	m = step(t, m, takeQueuedMsg{reply: reply})
	if taken := <-reply; taken != nil {
		t.Fatalf("command-headed queue must take nothing, got %+v", taken)
	}

	// Empty queue: nil.
	m.queue = nil
	reply = make(chan []Input, 1)
	m = step(t, m, takeQueuedMsg{reply: reply})
	if taken := <-reply; taken != nil {
		t.Fatalf("empty queue must take nothing, got %+v", taken)
	}
}
