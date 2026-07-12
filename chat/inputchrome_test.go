package chat

import (
	"strings"
	"testing"

	"chatchain/internal/readline"

	"github.com/fatih/color"
)

// TestComposerPromptStructure: the composer prompt is exactly three lines — a
// full-width separator rule, a status line, and the "❯ " marker — and the
// separator fills the width.
func TestComposerPromptStructure(t *testing.T) {
	p := &stubProvider{model: "gpt-test"}
	budget := newContextBudget(128_000)
	lines := strings.Split(stripANSI(composerPrompt(p, budget, 0, 40)), "\n")
	if len(lines) != 3 {
		t.Fatalf("composerPrompt lines = %d, want 3: %q", len(lines), lines)
	}
	if sep := lines[0]; sep != strings.Repeat("─", 40) {
		t.Errorf("separator = %q, want 40 ─", sep)
	}
	if !strings.Contains(lines[2], strings.TrimSpace(userPrompt)) {
		t.Errorf("last line %q missing the %q marker", lines[2], userPrompt)
	}
	if !strings.Contains(lines[1], "gpt-test") || !strings.Contains(lines[1], "ctx") {
		t.Errorf("status line %q missing model/ctx", lines[1])
	}
}

// TestComposerMarkerStableAcrossDraft pins the robustness invariant that keeps
// readline's cursor column (ppos) fixed: the final marker line must be byte-for-
// byte identical no matter how many draft tokens the status line reports.
func TestComposerMarkerStableAcrossDraft(t *testing.T) {
	p := &stubProvider{model: "m"}
	budget := newContextBudget(128_000)
	last := func(draft int) string {
		l := strings.Split(composerPrompt(p, budget, draft, 60), "\n")
		return l[len(l)-1]
	}
	if a, b := last(0), last(99_999); a != b {
		t.Errorf("marker line changed with draft: %q vs %q", a, b)
	}
}

// TestComposerStatusLineFitsWidth: the status never exceeds the terminal width
// (a wrapped status would add a row and desync the redraw geometry).
func TestComposerStatusLineFitsWidth(t *testing.T) {
	p := &stubProvider{model: strings.Repeat("long-model-name-", 8)}
	budget := newContextBudget(128_000)
	for _, w := range []int{10, 20, 40, 80} {
		if got := displayWidth(stripANSI(composerStatusLine(p, budget, 0, w))); got > w {
			t.Errorf("status width %d > terminal width %d", got, w)
		}
	}
}

// TestComposerStatusAlignmentAndColor: the status text is indented to the input
// column (past "❯ ") so it lines up under where typing begins, and the model and
// context fields carry distinct faint hues.
func TestComposerStatusAlignmentAndColor(t *testing.T) {
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = true })
	p := &stubProvider{model: "gpt-test"}
	budget := newContextBudget(128_000)

	line := composerStatusLine(p, budget, 0, 80)
	indent := strings.Repeat(" ", displayWidth(userPrompt))
	if !strings.HasPrefix(stripANSI(line), indent+"gpt-test") {
		t.Errorf("status not aligned to input column: %q", stripANSI(line))
	}
	// The model segment and the ctx segment must use different SGR codes so the
	// fields read as distinct (not one flat faint).
	modelSGR := StatusModelStyle.Sprint("x")
	ctxSGR := StatusCtxStyle.Sprint("x")
	if modelSGR == ctxSGR {
		t.Fatalf("model and ctx styles are identical")
	}
	if !strings.Contains(line, StatusModelStyle.Sprint("gpt-test")) {
		t.Errorf("model field not styled with StatusModelStyle: %q", line)
	}
	if !strings.Contains(stripANSI(line), "ctx ") {
		t.Errorf("ctx field missing: %q", stripANSI(line))
	}
}

// TestComposerStatusFallsBackToType: with no model chosen yet, the status shows
// the provider type rather than an empty slot.
func TestComposerStatusFallsBackToType(t *testing.T) {
	p := &stubProvider{model: ""}
	budget := newContextBudget(128_000)
	if !strings.Contains(stripANSI(composerStatusLine(p, budget, 0, 80)), "stub") {
		t.Errorf("empty model should fall back to provider type")
	}
}

// TestStatusWithDraft: draft tokens add to the used figure; a zero draft equals
// the plain status.
func TestStatusWithDraft(t *testing.T) {
	budget := newContextBudget(128_000)
	budget.used = 1000
	if got, want := budget.statusWithDraft(0), budget.status(); got != want {
		t.Errorf("statusWithDraft(0) = %q, want == status() %q", got, want)
	}
	big := budget.statusWithDraft(50_000)
	if !strings.Contains(big, "51k") { // 1000 + 50000 → 51k
		t.Errorf("statusWithDraft(50k) = %q, want it to include the draft (51k)", big)
	}
}

// TestComposerChromeListenerRepaint: the listener forces a repaint (ok=true)
// only when a real keystroke changes the rendered status, and never on the init
// call (key==0), which would clobber the empty buffer.
func TestComposerChromeListenerRepaint(t *testing.T) {
	p := &stubProvider{model: "m"}
	budget := newContextBudget(128_000)
	getNil := func() *readline.Instance { return nil }
	l := composerChromeListener(getNil, p, budget, nil)

	// init call: seeds the prompt, must not force a repaint.
	if _, _, ok := l(nil, 0, 0); ok {
		t.Errorf("init call returned ok=true, want false (would clobber buffer)")
	}
	// a real keystroke whose draft differs from the seed → repaint.
	line := []rune("this is a reasonably long draft message")
	if _, _, ok := l(line, len(line), 'e'); !ok {
		t.Errorf("status-changing keystroke returned ok=false, want true (repaint)")
	}
	// the same line again (same draft) → no repaint.
	if _, _, ok := l(line, len(line), 'x'); ok {
		t.Errorf("unchanged keystroke returned ok=true, want false (cheap path)")
	}
}

// TestComposerEraser verifies the unified composer-erasure contract: an armed
// eraser clears the composer on the first write (and only the first), a flush
// clears an armed-but-never-written composer, and a disarmed eraser is a plain
// pass-through — the behavior that lets interactive TUIs (which bypass this
// writer) keep the composer as a frame while printed output clears it in place.
func TestComposerEraser(t *testing.T) {
	// eraseComposer emits a cursor-up + clear sequence; detect it by the CSI "A".
	hasErase := func(s string) bool { return strings.Contains(s, "\x1b[") && strings.Contains(s, "A") }

	// Armed: the first write erases first, then passes the bytes through; the
	// second write does not erase again.
	var buf strings.Builder
	e := newComposerEraser(&buf)
	e.arm("hello")
	if _, err := e.Write([]byte("OUTPUT")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	first := buf.String()
	if !hasErase(first) || !strings.HasSuffix(first, "OUTPUT") {
		t.Errorf("first armed write = %q, want erase-then-OUTPUT", first)
	}
	buf.Reset()
	e.Write([]byte("MORE"))
	if got := buf.String(); hasErase(got) || got != "MORE" {
		t.Errorf("second write = %q, want plain MORE (no re-erase)", got)
	}

	// Flush erases an armed composer that never produced output.
	buf.Reset()
	e.arm("x")
	e.flush()
	if got := buf.String(); !hasErase(got) {
		t.Errorf("flush of armed eraser = %q, want an erase sequence", got)
	}
	// A second flush (now disarmed) is a no-op.
	buf.Reset()
	e.flush()
	if got := buf.String(); got != "" {
		t.Errorf("flush of disarmed eraser = %q, want empty", got)
	}
}

// TestComposerChromeListenerBaseWins: when the base listener rewrites the line
// (e.g. the slash-command full repaint), the composer wrapper defers to it.
func TestComposerChromeListenerBaseWins(t *testing.T) {
	p := &stubProvider{model: "m"}
	budget := newContextBudget(128_000)
	getNil := func() *readline.Instance { return nil }
	base := func(line []rune, pos int, key rune) ([]rune, int, bool) {
		return []rune("REWRITTEN"), 3, true
	}
	l := composerChromeListener(getNil, p, budget, base)
	nl, np, ok := l([]rune("/mo"), 3, 'o')
	if !ok || string(nl) != "REWRITTEN" || np != 3 {
		t.Errorf("base rewrite not honored: nl=%q np=%d ok=%v", string(nl), np, ok)
	}
}
