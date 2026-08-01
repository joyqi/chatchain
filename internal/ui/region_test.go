package ui

import (
	"strings"
	"testing"
	"time"
)

// A committed entry with embedded newlines must expand to one tail entry per
// visual row: all window bookkeeping (tail height, rebalance, overflow) — and
// through it the frame anchor and the composer cursor — assumes one row per
// entry. Seen live: a provider error's multi-line JSON body committed as one
// string pushed the real cursor rows up into the error text.
func TestRegionCommitSplitsEmbeddedNewlines(t *testing.T) {
	r := &region{emit: func([]string, regionMsg) {}}
	r.commit([]string{"Error: 400 {\n  \"message\": \"bad\"\n}"})
	want := []string{"Error: 400 {", "  \"message\": \"bad\"", "}"}
	if len(r.tail) != 3 || r.tail[0] != want[0] || r.tail[1] != want[1] || r.tail[2] != want[2] {
		t.Fatalf("tail = %q, want %q", r.tail, want)
	}
}

// Overwide entries are hard-wrapped on commit: staged in the frame they would
// be CLIPPED by the cell grid while scrollback soft-wraps them — the class of
// "the first error shows truncated, then wraps once it scrolls out". Wrapping
// targets width-1 (sanitizeOverflow eats the last character of exact-multiple
// rows), and rows that already fit — like UserBlock's full-width rows — pass
// through untouched.
func TestRegionCommitWrapsToScreenWidth(t *testing.T) {
	var scroll []string
	r := &region{u: &UI{}, emit: func(over []string, snap regionMsg) {
		scroll = append(scroll, over...)
	}}
	r.u.width.Store(10)

	r.commit([]string{"aaaaaaaaaabbbbbbbbbbcc", "\x1b[31mdddddddddddd\x1b[0m", "fits-fine"})
	want := []string{
		"aaaaaaaaa", "abbbbbbbb", "bbcc",
		"\x1b[31mddddddddd", "\x1b[31mddd\x1b[0m",
		"fits-fine",
	}
	all := append(append([]string{}, scroll...), r.tail...)
	if len(all) != len(want) {
		t.Fatalf("rows = %q, want %q", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, all[i], want[i])
		}
	}
}

// Width 0 (startup before the first resize, emit-seam tests) skips wrapping.
func TestRegionCommitNoWidthNoWrap(t *testing.T) {
	r := &region{emit: func([]string, regionMsg) {}}
	r.commit([]string{strings.Repeat("x", 500)})
	if len(r.tail) != 1 {
		t.Fatalf("tail rows = %d, want 1", len(r.tail))
	}
}

// The one-row invariant on the preview side: labels (fresh open AND the
// in-place relabel), rolling preview lines, and the status-row detail are
// each rendered — and counted in the cursor offset — as exactly one frame
// row, so embedded line breaks collapse to spaces on entry.
func TestRegionPreviewEntriesCollapseToOneRow(t *testing.T) {
	r := &region{emit: func([]string, regionMsg) {}}

	r.openCallPreview("[bash\ncommand:a]") // fresh open
	if r.label != "[bash command:a]" {
		t.Fatalf("fresh label = %q", r.label)
	}
	r.openCallPreview("[bash\r\ncommand:b]") // ensure branch: relabel in place
	if r.label != "[bash command:b]" {
		t.Fatalf("relabel = %q", r.label)
	}
	r.setCallDetail("1.2k\ntokens")
	if r.detail != "1.2k tokens" {
		t.Fatalf("detail = %q", r.detail)
	}

	r.openPreview("rendering\ntable…") // plain preview label
	if r.label != "rendering table…" {
		t.Fatalf("plain label = %q", r.label)
	}
	r.previewLine("|a|\n|b|")
	if len(r.ptail) != 1 || r.ptail[0] != "|a| |b|" {
		t.Fatalf("ptail = %q", r.ptail)
	}
}

// Replays the region op sequence of a full two-round tool turn (thinking
// widget → pending call raise → settle → results → next round) and pins the
// scrollback stream (overflow ∪ final tail): exactly one blank separator per
// block boundary, no widget row ever leaking into scrollback.
func TestRegionTwoRoundToolTurn(t *testing.T) {
	var scroll []string
	r := &region{emit: func(over []string, snap regionMsg) {
		scroll = append(scroll, over...)
	}}

	// round 1
	r.commit([]string{"❯ user prompt"})       // user block
	r.commit([]string{""})                    // openThinking separator
	r.openCallPreview("Thinking")             // thinking widget
	r.closePreview()                          // settleThinking
	r.commit([]string{"◇ thought for <1s A"}) // marker
	r.commit([]string{""})                    // pendingCall separator
	r.openCallPreview("[bash …]")             // composing raise
	r.openCallPreview("[bash command:pwd]")   // relabel in place
	r.closePreview()                          // settleCall
	r.commit([]string{"[bash command:pwd]"})  // header
	r.commit([]string{"  ⎿ /Users/joyqi"})    // toolLines

	// round 2
	r.commit([]string{""}) // openThinking separator
	r.openCallPreview("Thinking")
	r.closePreview()
	r.commit([]string{"◇ thought for <1s B"})
	r.commit([]string{""})
	r.commit([]string{"final content"})

	r.flush()
	scroll = append(scroll, r.tail...)

	got := strings.Join(scroll, "\n")
	want := strings.Join([]string{
		"❯ user prompt",
		"",
		"◇ thought for <1s A",
		"",
		"[bash command:pwd]",
		"  ⎿ /Users/joyqi",
		"",
		"◇ thought for <1s B",
		"",
		"final content",
	}, "\n")
	if got != want {
		t.Fatalf("scrollback stream:\n%q\nwant:\n%q", got, want)
	}
}

// The call clock pauses while the user is consulted (approval prompts,
// interactive tools): pausedAt freezes the elapsed figure, resume shifts
// `since` forward by the paused span so the figure continues where it froze,
// and every widget teardown clears the pause state.
func TestRegionClockPause(t *testing.T) {
	var snaps []regionMsg
	r := &region{emit: func(_ []string, snap regionMsg) { snaps = append(snaps, snap) }}

	r.pauseClock() // no call preview: a no-op, publishes nothing
	if len(snaps) != 0 {
		t.Fatalf("pause without a preview must be silent, got %d snapshots", len(snaps))
	}

	r.openCallPreview("[edit_file …]")
	before := r.since
	r.pauseClock()
	if r.pausedAt.IsZero() {
		t.Fatal("pausedAt not set")
	}
	frozen := snaps[len(snaps)-1]
	if frozen.pausedAt.IsZero() {
		t.Fatal("snapshot must carry pausedAt for the model to freeze the figure")
	}
	r.pauseClock() // idempotent: already paused
	_ = r.pausedAt

	time.Sleep(5 * time.Millisecond)
	r.resumeClock()
	if !r.pausedAt.IsZero() {
		t.Fatal("resume must clear pausedAt")
	}
	if !r.since.After(before) {
		t.Fatalf("since must shift forward by the paused span (was %v, now %v)", before, r.since)
	}

	r.resumeClock() // idempotent: not paused

	r.pauseClock()
	r.dropPreview() // teardown clears the pause with the widget
	if !r.pausedAt.IsZero() {
		t.Fatal("dropPreview must clear pausedAt")
	}
}
