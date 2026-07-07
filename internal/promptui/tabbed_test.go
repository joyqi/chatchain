package promptui

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"chatchain/internal/readline"
)

// feed sends a sequence of keys to a panel's HandleKey, returning the consumed
// flags in order (handy for asserting directory-descend vs file-choose).
func feed(p Panel, keys ...rune) []bool {
	out := make([]bool, len(keys))
	for i, k := range keys {
		out[i] = p.HandleKey(k)
	}
	return out
}

func TestListPanelSingleSelect(t *testing.T) {
	p := NewListPanel("Resume", []string{"a", "b", "c"}, false)
	// Down twice, then up once → cursor at index 1.
	feed(p, KeyNext, KeyNext, KeyPrev)
	if p.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1", p.Cursor())
	}
	if got := p.Selected(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("Selected = %v, want [1]", got)
	}
	// Enter is never consumed by the panel (the container commits).
	if p.HandleKey(KeyEnter) {
		t.Fatal("ListPanel consumed Enter; want container to commit")
	}
}

func TestListPanelVimAndBounds(t *testing.T) {
	p := NewListPanel("x", []string{"a", "b"}, false)
	// k at the top is a no-op; j moves down; j past the end clamps.
	feed(p, 'k')
	if p.Cursor() != 0 {
		t.Fatalf("cursor after k at top = %d, want 0", p.Cursor())
	}
	feed(p, 'j', 'j', 'j')
	if p.Cursor() != 1 {
		t.Fatalf("cursor after jjj = %d, want 1 (clamped)", p.Cursor())
	}
	// g/G jump to top/bottom.
	feed(p, 'g')
	if p.Cursor() != 0 {
		t.Fatalf("cursor after g = %d, want 0", p.Cursor())
	}
	feed(p, 'G')
	if p.Cursor() != 1 {
		t.Fatalf("cursor after G = %d, want 1", p.Cursor())
	}
}

func TestListPanelMultiSelect(t *testing.T) {
	p := NewListPanel("Delete", []string{"a", "b", "c", "d"}, true)
	// Check row 0, move to row 2 and check it, then toggle row 2 off and on.
	feed(p, ' ')                   // check 0
	feed(p, KeyNext, KeyNext, ' ') // check 2
	if got := p.Selected(); !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatalf("Selected = %v, want [0 2]", got)
	}
	feed(p, ' ') // uncheck 2
	if got := p.Selected(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("Selected after uncheck = %v, want [0]", got)
	}
	// In single mode Space would not toggle; here it must be consumed.
	if !p.HandleKey(' ') {
		t.Fatal("multi ListPanel did not consume Space")
	}
}

func TestListPanelEmpty(t *testing.T) {
	p := NewListPanel("x", nil, false)
	if got := p.Selected(); got != nil {
		t.Fatalf("Selected on empty = %v, want nil", got)
	}
	// Render must not panic and returns a placeholder row.
	if lines := p.Render(40, 5); len(lines) != 1 {
		t.Fatalf("empty Render lines = %d, want 1", len(lines))
	}
}

func TestBrowserPanelDescendAndChoose(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "note.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewBrowserPanel("Add", root)
	b.Render(40, 10) // force the initial scan
	// Entries: "../" then "sub/". Move to "sub/" and Enter to descend.
	// (root has a parent, so index 0 is "../", index 1 is "sub/".)
	feed(b, KeyNext) // highlight sub/
	if consumed := b.HandleKey(KeyEnter); !consumed {
		t.Fatal("Enter on a directory should be consumed (descend)")
	}
	if b.dir != sub {
		t.Fatalf("dir after descend = %q, want %q", b.dir, sub)
	}
	if b.Chosen() != "" {
		t.Fatalf("Chosen after descend = %q, want empty", b.Chosen())
	}
	// Inside sub: "../" (index 0) then "note.txt" (index 1). Choose the file.
	feed(b, KeyNext)
	if consumed := b.HandleKey(KeyEnter); consumed {
		t.Fatal("Enter on a file should NOT be consumed (let container submit)")
	}
	if b.Chosen() != file {
		t.Fatalf("Chosen = %q, want %q", b.Chosen(), file)
	}
}

func TestBrowserPanelParentNav(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewBrowserPanel("Add", sub)
	b.Render(40, 10)
	// Index 0 is "../" → Enter descends up to root.
	if consumed := b.HandleKey(KeyEnter); !consumed {
		t.Fatal("Enter on ../ should be consumed")
	}
	if b.dir != root {
		t.Fatalf("dir after ../ = %q, want %q", b.dir, root)
	}
}

func TestTruncateCJK(t *testing.T) {
	// Two-column CJK runes: width 5 fits 你好 (4 cols) + ellipsis.
	got := truncate("你好世界", 5, cjkWidth)
	if got != "你好…" {
		t.Fatalf("truncate = %q, want %q", got, "你好…")
	}
	if truncate("abc", 10, cjkWidth) != "abc" {
		t.Fatal("short strings should pass through unchanged")
	}
	if truncate("abc", 0, cjkWidth) != "" {
		t.Fatal("width 0 should truncate to empty")
	}
}

func TestTabBarCJKWidth(t *testing.T) {
	// CJK titles are 2 columns per rune via the injected width func, so "甲乙丙"
	// measures 6 columns — a width-6 bar fits exactly one tab and drops the rest.
	tb := &Tabbed{
		Panels:    []Panel{NewListPanel("甲乙丙", nil, true), NewListPanel("丁戊己", nil, false)},
		RuneWidth: cjkWidth,
	}
	// Wide enough for both tabs: both titles present.
	bar := tb.tabBar(40)
	if !bytes.Contains([]byte(bar), []byte("甲乙丙")) || !bytes.Contains([]byte(bar), []byte("丁戊己")) {
		t.Fatalf("tab bar missing a title: %q", bar)
	}
	// Too narrow for the second tab: it is dropped, the first survives.
	narrow := tb.tabBar(6) // 甲乙丙 = 6 cols exactly, no room for a separator + next tab
	if bytes.Contains([]byte(narrow), []byte("丁戊己")) {
		t.Fatalf("narrow tab bar should have dropped the second tab: %q", narrow)
	}
}

// scriptedStdin is a ReadCloser that yields a fixed byte script then blocks
// forever, so the container's readline loop consumes exactly our keys.
type scriptedStdin struct {
	data []byte
	pos  int
	done chan struct{}
}

func newScriptedStdin(script []byte) *scriptedStdin {
	return &scriptedStdin{data: script, done: make(chan struct{})}
}

func (s *scriptedStdin) Read(p []byte) (int, error) {
	if s.pos < len(s.data) {
		n := copy(p, s.data[s.pos:])
		s.pos += n
		return n, nil
	}
	<-s.done // block until closed so readline keeps waiting
	return 0, nil
}

func (s *scriptedStdin) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// nopWriteCloser adapts a bytes.Buffer to io.WriteCloser for Stdout injection.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func TestTabbedRunTabSwitchAndCommit(t *testing.T) {
	// Script: Tab (switch to panel 1), then Enter (commit). Enter is CharEnter.
	script := []byte{readline.CharTab, readline.CharEnter}
	stdin := newScriptedStdin(script)
	var out bytes.Buffer

	p0 := NewListPanel("A", []string{"a"}, false)
	p1 := NewListPanel("B", []string{"b", "c"}, false)
	tb := &Tabbed{
		Panels:    []Panel{p0, p1},
		Stdin:     stdin,
		Stdout:    nopWriteCloser{&out},
		RuneWidth: cjkWidth,
		// Force a terminal-independent size so the loop renders deterministically.
	}

	focused, err := tb.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if focused != 1 {
		t.Fatalf("focused = %d, want 1 (after one Tab)", focused)
	}
}

func TestTabbedRunInterrupt(t *testing.T) {
	// Ctrl+C cancels with ErrInterrupt.
	stdin := newScriptedStdin([]byte{readline.CharInterrupt})
	var out bytes.Buffer
	tb := &Tabbed{
		Panels: []Panel{NewListPanel("A", []string{"a"}, false)},
		Stdin:  stdin,
		Stdout: nopWriteCloser{&out},
	}
	if _, err := tb.Run(); err != ErrInterrupt {
		t.Fatalf("Run err = %v, want ErrInterrupt", err)
	}
}

func TestTabbedRunQuit(t *testing.T) {
	// 'q' cancels like Ctrl+C / Esc, matching Select's keymap.
	stdin := newScriptedStdin([]byte("q"))
	var out bytes.Buffer
	tb := &Tabbed{
		Panels: []Panel{NewListPanel("A", []string{"a"}, false)},
		Stdin:  stdin,
		Stdout: nopWriteCloser{&out},
	}
	if _, err := tb.Run(); err != ErrInterrupt {
		t.Fatalf("Run err = %v, want ErrInterrupt ('q' should quit)", err)
	}
}

func TestListPanelPaging(t *testing.T) {
	items := make([]string, 20)
	for i := range items {
		items[i] = string(rune('a' + i%26))
	}
	p := NewListPanel("x", items, false)
	p.Render(40, 5) // establish a 5-row window for paging

	// ← / → (and vim h/l) move a full window at a time, clamped to the bounds.
	feed(p, KeyForward)
	if p.Cursor() != 5 {
		t.Fatalf("cursor after page down = %d, want 5", p.Cursor())
	}
	feed(p, 'l', 'l', 'l', 'l') // page down past the end → clamp at last
	if p.Cursor() != len(items)-1 {
		t.Fatalf("cursor after repeated page down = %d, want %d", p.Cursor(), len(items)-1)
	}
	feed(p, KeyBackward)
	if want := len(items) - 1 - 5; p.Cursor() != want {
		t.Fatalf("cursor after page up = %d, want %d", p.Cursor(), want)
	}
	feed(p, 'h', 'h', 'h', 'h') // page up past the top → clamp at 0
	if p.Cursor() != 0 {
		t.Fatalf("cursor after repeated page up = %d, want 0", p.Cursor())
	}
}

func TestViewPanelScrollAndWrap(t *testing.T) {
	// With Wrap, "short" is one row and the long line soft-wraps at width 10 into
	// two rows, so the wrapped content is ["short", "0123456789", "ABCDEFGHIJ"].
	p := NewViewPanel("Tools", []string{"short", "0123456789ABCDEFGHIJ"})
	p.Wrap = true
	rows := p.Render(10, 2) // window height 2 over 3 wrapped rows
	if len(rows) != 2 || rows[0] != "short" || rows[1] != "0123456789" {
		t.Fatalf("initial view = %v, want [short 0123456789]", rows)
	}
	// → pages down one window; voff clamps so the last wrapped row is visible.
	feed(p, KeyForward)
	rows = p.Render(10, 2)
	if rows[len(rows)-1] != "ABCDEFGHIJ" {
		t.Fatalf("after page down = %v, want to reach the last wrapped row", rows)
	}
	// G jumps to the bottom, g back to the top.
	feed(p, 'G')
	if got := p.Render(10, 2); got[len(got)-1] != "ABCDEFGHIJ" {
		t.Fatalf("after G = %v, want bottom", got)
	}
	feed(p, 'g')
	if got := p.Render(10, 2); got[0] != "short" {
		t.Fatalf("after g = %v, want top", got)
	}
	// Enter is not consumed — the container closes the read-only view.
	if p.HandleKey(KeyEnter) {
		t.Fatal("ViewPanel consumed Enter; want container to close")
	}
}

func TestViewPanelPan(t *testing.T) {
	// Default (no Wrap): long lines are clipped and h/l pans horizontally.
	p := NewViewPanel("Tools", []string{"0123456789ABCDEFGHIJ"})
	if got := p.Render(10, 1); got[0] != "0123456789" {
		t.Fatalf("initial = %q, want 0123456789", got[0])
	}
	feed(p, 'l', 'l', 'l', 'l', 'l') // pan right 5 columns → cols [5,15)
	if got := p.Render(10, 1); got[0] != "56789ABCDE" {
		t.Fatalf("after pan right = %q, want 56789ABCDE", got[0])
	}
	feed(p, 'h', 'h', 'h', 'h', 'h', 'h', 'h') // pan back left, clamped at 0
	if got := p.Render(10, 1); got[0] != "0123456789" {
		t.Fatalf("after pan left = %q, want 0123456789", got[0])
	}
}
