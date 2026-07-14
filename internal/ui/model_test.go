package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	m := newModel(&w)
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

// TestPasteTags: a multi-line paste collapses to a [#N …] tag in the composer;
// submit expands it in Text while Display keeps the tag.
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
	if !strings.Contains(r.in.Display, "[#1") {
		t.Fatalf("Display = %q, want the tag preserved", r.in.Display)
	}
	// Single-line pastes insert verbatim.
	m = step(t, m, tea.PasteMsg{Content: "inline"})
	if got := m.ta.Value(); got != "inline" {
		t.Fatalf("single-line paste = %q", got)
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
