package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// --- matching ---------------------------------------------------------------

// TestAsciiFoldPreservesByteLength is the invariant the whole offset scheme
// rests on: match offsets index back into the ORIGINAL string, so folding must
// never change how many bytes anything takes. (strings.ToLower cannot promise
// this — U+0130 lowercases to two runes.)
func TestAsciiFoldPreservesByteLength(t *testing.T) {
	for _, s := range []string{"Hello", "ÄÖÜ", "İstanbul", "中文 MiXeD", "ß"} {
		if got, want := len(asciiFold(s)), len(s); got != want {
			t.Errorf("asciiFold(%q) length = %d, want %d", s, got, want)
		}
	}
	if got := asciiFold("AbC-XyZ"); got != "abc-xyz" {
		t.Errorf("asciiFold = %q", got)
	}
}

func TestMatchRanges(t *testing.T) {
	cases := []struct {
		name, text, query string
		want              [][2]int
	}{
		{"simple", "hello world", "world", [][2]int{{6, 11}}},
		{"case insensitive", "Hello World", "hello", [][2]int{{0, 5}}},
		{"repeated non-overlapping", "aaaa", "aa", [][2]int{{0, 2}, {2, 4}}},
		{"no match", "hello", "zzz", nil},
		{"query longer than text", "hi", "hello", nil},
		{"empty query", "hello", "", nil},
		{"cjk", "中文测试", "文测", [][2]int{{3, 9}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := matchRanges(tt.text, tt.query)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("matchRanges(%q, %q) = %v, want %v", tt.text, tt.query, got, tt.want)
			}
		})
	}
}

// --- highlighting -----------------------------------------------------------

// TestHighlightLinePlain checks the basic wrap: the hit is reversed, the rest
// of the line is untouched, and stripping the SGR gives the original back.
func TestHighlightLinePlain(t *testing.T) {
	got := highlightLine("hello world", "world", -1)
	if !strings.Contains(got, searchHitSGR+"world") {
		t.Errorf("hit not highlighted: %q", got)
	}
	if stripSGRText(got) != "hello world" {
		t.Errorf("text altered: %q", stripSGRText(got))
	}
}

// TestHighlightLineKeepsOwnColor is the reason this cannot be a string
// replace: a line that carries its own SGR (a /debug request row, a coloured
// body) must still be coloured AFTER the highlight closes. A bare reset would
// strip the rest of the line bare.
func TestHighlightLineKeepsOwnColor(t *testing.T) {
	line := cyan + "error: file not found" + sgrReset
	got := highlightLine(line, "file", -1)
	if stripSGRText(got) != "error: file not found" {
		t.Fatalf("text altered: %q", stripSGRText(got))
	}
	// After the highlight's reset, the line's own cyan must be re-asserted.
	after := got[strings.Index(got, "file")+len("file"):]
	if !strings.Contains(after, cyan) {
		t.Errorf("line colour not replayed after the hit: %q", got)
	}
}

// TestHighlightLineNeverSplitsEscapes: offsets are computed on the plain text,
// so an escape sequence sitting inside a match must survive whole.
func TestHighlightLineNeverSplitsEscapes(t *testing.T) {
	line := "abc" + cyan + "def"
	got := highlightLine(line, "cd", -1)
	if !strings.Contains(got, cyan) {
		t.Errorf("escape mangled: %q", got)
	}
	if stripSGRText(got) != "abcdef" {
		t.Errorf("text altered: %q", stripSGRText(got))
	}
}

// TestHighlightLineCurrentHit: the hit n/p is parked on is brighter than the
// others.
func TestHighlightLineCurrentHit(t *testing.T) {
	got := highlightLine("foo bar foo", "foo", 8)
	if !strings.Contains(got, searchCurSGR) {
		t.Errorf("current hit not distinguished: %q", got)
	}
	if strings.Count(got, searchHitSGR) < 1 {
		t.Errorf("other hit not highlighted: %q", got)
	}
}

func TestHighlightLineNoMatchReturnsInput(t *testing.T) {
	line := cyan + "hello" + sgrReset
	if got := highlightLine(line, "zzz", -1); got != line {
		t.Errorf("no-match line rewritten: %q", got)
	}
}

// --- row-panel filtering ----------------------------------------------------

// openSearchList opens a filterable list long enough for "/" to be live.
func openSearchList(t *testing.T, m *model, items []string, reply chan TabbedResult) *model {
	t.Helper()
	return step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelList, Items: items, Search: true},
	}}, reply: reply})
}

func longItems(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("item-%02d", i)
	}
	return out
}

// TestSearchGatedByFlagAndOverflow: "/" is inert without the opt-in, and inert
// on a list that already fits on screen — there is nothing to search for when
// every option is visible.
func TestSearchGatedByFlagAndOverflow(t *testing.T) {
	cases := []struct {
		name   string
		panel  Panel
		items  int
		wantOK bool
	}{
		{"flag off, long list", Panel{Kind: PanelList}, 40, false},
		{"flag on, short list", Panel{Kind: PanelList, Search: true}, 3, false},
		{"flag on, long list", Panel{Kind: PanelList, Search: true}, 40, true},
		{"view ignores the flag", Panel{Kind: PanelView}, 40, true},
		{"view, short body", Panel{Kind: PanelView}, 3, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.panel
			items := longItems(tt.items)
			if p.Kind == PanelView {
				p.Lines = items
			} else {
				p.Items = items
			}
			s := newSurface(TabbedSpec{Panels: []Panel{p}}, nil, 1)
			if got := s.ps[0].searchAvailable(p); got != tt.wantOK {
				t.Errorf("searchAvailable = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

// TestSearchFiltersLive narrows the list as the query is typed — before Enter,
// so a query that matches nothing is visible immediately.
func TestSearchFiltersLive(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, longItems(40), reply)
	st := &m.surf.ps[0]
	if len(st.view) != 40 {
		t.Fatalf("unfiltered view = %d rows, want 40", len(st.view))
	}

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	if st.search.mode != searchTyping {
		t.Fatal("\"/\" did not open the query field")
	}
	m = typeText(t, m, "item-1")
	if len(st.view) != 10 { // item-10 … item-19
		t.Errorf("live filter kept %d rows, want 10", len(st.view))
	}
}

// TestSearchCursorStaysUnderlyingIndex is the contract callers depend on:
// every caller reads Cursor back as an index into the ORIGINAL Items
// (modelValues[r.Panels[0].Cursor]), so filtering must not renumber it.
func TestSearchCursorStaysUnderlyingIndex(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, longItems(40), reply)

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-3")
	m = enter(t, m)                                             // apply the filter
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})) // second match: item-31
	m = enter(t, m)                                             // commit

	r := <-reply
	if r.Cancelled {
		t.Fatal("surface cancelled")
	}
	if r.Panels[0].Cursor != 31 {
		t.Errorf("Cursor = %d, want 31 (the index into the ORIGINAL items)", r.Panels[0].Cursor)
	}
}

// TestSearchClearRestoresList: "c" lifts the filter, the panel's own keys were
// never taken away.
func TestSearchClearRestoresList(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, longItems(40), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-1")
	m = enter(t, m)
	if len(st.view) != 10 {
		t.Fatalf("filter not applied: %d rows", len(st.view))
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'c', Text: "c"}))
	if st.search.mode != searchOff || len(st.view) != 40 {
		t.Errorf("after clear: mode=%v rows=%d, want off/40", st.search.mode, len(st.view))
	}
}

// TestSearchEscapeInFieldLeavesSearch: ESC in the query field drops the search
// entirely rather than applying a half-typed query.
func TestSearchEscapeInFieldLeavesSearch(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, longItems(40), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-1")
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.surf == nil {
		t.Fatal("ESC in the query field closed the whole surface")
	}
	if st.search.mode != searchOff || len(st.view) != 40 {
		t.Errorf("after ESC: mode=%v rows=%d, want off/40", st.search.mode, len(st.view))
	}
}

// TestSearchNoMatchShowsEverything: a filter that empties the list would leave
// a cursor pointing at nothing that Enter could still commit, so no-match
// falls back to the whole list and says so.
func TestSearchNoMatchShowsEverything(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, longItems(40), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "zzzz")
	if len(st.view) != 40 {
		t.Errorf("no-match view = %d rows, want the full 40", len(st.view))
	}
	// The fallback must stay distinguishable from "matched everything", or the
	// hint row would claim 40 of 40 matches for a query that found none.
	if _, ok := st.filteredCount(m.surf.spec.Panels[0]); ok {
		t.Error("filteredCount reported a match for a query that found none")
	}
	if !strings.Contains(content(m), "no match") {
		t.Error("the query row does not say the search found nothing")
	}
}

// TestSearchMultiChecksSurviveFiltering: Multi checks are recorded against
// underlying indices, so options checked under one query are still submitted
// after another query hides them.
func TestSearchMultiChecksSurviveFiltering(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Flags", Kind: PanelMulti, Items: longItems(40), Search: true},
	}}, reply: reply})

	// Check item-05 through one filter…
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-05")
	m = enter(t, m)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Text: " "}))
	// …then switch to a filter that hides it and check item-22.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-22")
	m = enter(t, m)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Text: " "}))
	m = enter(t, m)

	r := <-reply
	want := []int{5, 22}
	if fmt.Sprint(r.Panels[0].Checked) != fmt.Sprint(want) {
		t.Errorf("Checked = %v, want %v (a hidden check must still commit)", r.Panels[0].Checked, want)
	}
}

// TestSearchKeepsCustomRowVisible: "Other…" is the way to answer when no
// option fits, so a filter must never hide it.
func TestSearchKeepsCustomRowVisible(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Model", Kind: PanelList, Items: longItems(40), Custom: true, Search: true},
	}}, reply: reply})
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "item-1")
	otherIdx := 40
	found := false
	for _, i := range st.view {
		if i == otherIdx {
			found = true
		}
	}
	if !found {
		t.Errorf("the Other… row was filtered away: view=%v", st.view)
	}
}

// TestSearchMatchesThroughStyling: rows can arrive pre-coloured (the /debug
// request list), and the query must be matched against what the user SEES,
// not against the escape bytes carrying it.
func TestSearchMatchesThroughStyling(t *testing.T) {
	items := longItems(40)
	items[7] = cyan + "item-07" + sgrReset + faint + " (styled)" + sgrReset
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchList(t, m, items, reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "styled")
	if len(st.view) != 1 || st.view[0] != 7 {
		t.Errorf("view = %v, want just the styled row (index 7)", st.view)
	}
	// And the escape bytes themselves must not be searchable.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "36m")
	if len(st.view) != 40 {
		t.Errorf("an SGR fragment matched %d rows; escapes must be invisible to search", len(st.view))
	}
}

// TestBrowserDescendClearsFilter: the query was aimed at the directory being
// left, so it must not silently hide half of the new one.
func TestBrowserDescendClearsFilter(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "alpha")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("inner-%02d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Add", Kind: PanelBrowser, Dir: dir, Search: true},
	}}, reply: reply})
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "alpha")
	m = enter(t, m) // apply: only the subdirectory remains
	if len(st.view) != 1 {
		t.Fatalf("filter kept %d rows, want 1", len(st.view))
	}
	m = enter(t, m) // descend into it
	if st.search.mode != searchOff {
		t.Errorf("descending kept the filter: mode=%v", st.search.mode)
	}
	if len(st.view) != len(st.entries) {
		t.Errorf("view = %d rows, entries = %d: the new directory is still filtered",
			len(st.view), len(st.entries))
	}
}

// --- View search ------------------------------------------------------------

func openSearchView(t *testing.T, m *model, lines []string, reply chan TabbedResult) *model {
	t.Helper()
	return step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Body", Kind: PanelView, Lines: lines},
	}}, reply: reply})
}

func viewBody(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d: nothing here", i)
	}
	out[5] = "line 5: needle one"
	out[30] = "line 30: needle two"
	return out
}

// TestViewSearchCollectsAndCenters: Enter enters the walker parked on the
// first hit, and the landing line sits near the middle of the window rather
// than scraping the top or bottom edge.
func TestViewSearchCollectsAndCenters(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchView(t, m, viewBody(60), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	if st.search.mode != searchApplied {
		t.Fatalf("Enter did not enter the walker: mode=%v", st.search.mode)
	}
	if len(st.search.hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(st.search.hits))
	}
	_ = content(m) // render resolves the pending jump
	// The first hit sits at line 5, closer to the top than half a window, so
	// centring clamps to the top rather than scrolling past the start.
	if st.offset != 0 {
		t.Errorf("offset = %d, want 0 (a hit near the top clamps)", st.offset)
	}
	// The second is far enough down to actually land mid-window.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
	_ = content(m)
	if want := 30 - st.rows/2; st.offset != want {
		t.Errorf("offset = %d, want %d (hit line 30 centred in %d rows)", st.offset, want, st.rows)
	}
}

// TestViewSearchStepWraps: n walks forward through the hits and wraps at the
// end; p walks back.
func TestViewSearchStepWraps(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchView(t, m, viewBody(60), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
	if st.search.hitIdx != 1 {
		t.Errorf("after n: hitIdx = %d, want 1", st.search.hitIdx)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
	if st.search.hitIdx != 0 {
		t.Errorf("n past the last hit must wrap: hitIdx = %d, want 0", st.search.hitIdx)
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'p', Text: "p"}))
	if st.search.hitIdx != 1 {
		t.Errorf("p before the first hit must wrap: hitIdx = %d, want 1", st.search.hitIdx)
	}
}

// TestViewSearchSurvivesRefresh: a live panel (/tools, /debug refresh twice a
// second) must not yank the walker back to the first hit under the reader —
// with the walker reset every tick, n could never reach the third hit.
func TestViewSearchSurvivesRefresh(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	body := viewBody(60)
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{
		RefreshEvery: 500,
		Panels: []Panel{{
			Title: "Tools", Kind: PanelView, Lines: body,
			Refresh: func() []string { return body },
		}},
	}, reply: reply})
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
	_ = content(m)
	wantIdx, wantOff := st.search.hitIdx, st.offset

	m = step(t, m, surfTickMsg{gen: m.surfGen})
	_ = content(m)
	if st.search.hitIdx != wantIdx {
		t.Errorf("a refresh moved the walker: hitIdx = %d, want %d", st.search.hitIdx, wantIdx)
	}
	if st.offset != wantOff {
		t.Errorf("a refresh scrolled the body: offset = %d, want %d", st.offset, wantOff)
	}
}

// TestViewSearchRefreshReanchorsOnContentShift: the walker is anchored to the
// HIT, not to its ordinal — content appearing above it renumbers every index
// while the reader is still looking at the same match.
func TestViewSearchRefreshReanchorsOnContentShift(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	body := viewBody(60)
	live := body
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{
		RefreshEvery: 500,
		Panels: []Panel{{
			Title: "Log", Kind: PanelView, Lines: live,
			Refresh: func() []string { return live },
		}},
	}, reply: reply})
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: 'n', Text: "n"}))
	parked := *st.currentHit() // the second hit, at line 30

	// A new matching row arrives at the top: every hit index shifts by one.
	live = append([]string{"line -1: needle zero"}, body...)
	m = step(t, m, surfTickMsg{gen: m.surfGen})

	if len(st.search.hits) != 3 {
		t.Fatalf("hits = %d, want 3 after the new row", len(st.search.hits))
	}
	got := st.currentHit()
	if got == nil || got.line != parked.line+1 {
		t.Errorf("walker landed on %+v, want the same match, now at line %d", got, parked.line+1)
	}
}

// TestViewSearchEscapeLadder: from the walker, q/Esc reopens the query field
// (the user is refining, not leaving), and Esc there drops the search.
func TestViewSearchEscapeLadder(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchView(t, m, viewBody(60), reply)
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if st.search.mode != searchTyping {
		t.Fatalf("ESC from the walker: mode=%v, want typing", st.search.mode)
	}
	if m.surf == nil {
		t.Fatal("ESC from the walker closed the surface")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if st.search.mode != searchOff {
		t.Errorf("ESC in the field: mode=%v, want off", st.search.mode)
	}
	if m.surf == nil {
		t.Fatal("ESC in the field closed the surface")
	}
	// Only now does ESC reach the surface itself.
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.surf != nil {
		t.Error("a third ESC should close the surface")
	}
}

// TestViewSearchHighlightsInBody: the rendered panel carries the highlight,
// and dropping the search takes it away again.
func TestViewSearchHighlightsInBody(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	m = openSearchView(t, m, viewBody(60), reply)

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	if !strings.Contains(content(m), searchCurSGR) {
		t.Error("the current hit is not highlighted in the rendered body")
	}
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})) // → typing
	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})) // → off
	if strings.Contains(content(m), searchCurSGR) {
		t.Error("the highlight survived leaving search")
	}
}

// TestViewSearchWrapCenteringUsesWrappedRows: with Wrap on, offset counts
// WRAPPED rows while hits are recorded against logical lines — the jump has to
// convert, or long lines send it to the wrong place.
func TestViewSearchWrapCenteringUsesWrappedRows(t *testing.T) {
	m := newTestModel(t)
	reply := make(chan TabbedResult, 1)
	long := strings.Repeat("padding ", 30) // wraps to several rows at width 80
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = long
	}
	lines[20] = "the needle is here"
	m = step(t, m, tabbedOpenMsg{spec: TabbedSpec{Panels: []Panel{
		{Title: "Body", Kind: PanelView, Wrap: true, Lines: lines},
	}}, reply: reply})
	st := &m.surf.ps[0]

	m = step(t, m, tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
	m = typeText(t, m, "needle")
	m = enter(t, m)
	_ = content(m)

	if len(st.wrapStarts) != len(lines) {
		t.Fatalf("wrapStarts = %d entries, want %d", len(st.wrapStarts), len(lines))
	}
	wantRow := st.wrapStarts[20]
	if wantRow <= 20 {
		t.Fatalf("test is not exercising wrapping: logical line 20 starts at row %d", wantRow)
	}
	if want := wantRow - st.rows/2; st.offset != want {
		t.Errorf("offset = %d, want %d (wrapped row %d centred)", st.offset, want, wantRow)
	}
}
