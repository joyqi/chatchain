package ui

import (
	"strings"
	"testing"
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
