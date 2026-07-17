package ui

import (
	"strings"
	"testing"
)

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
