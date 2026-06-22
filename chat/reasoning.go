package chat

import (
	"fmt"
	"os"
	"time"

	"chatchain/internal/promptui"

	"github.com/briandowns/spinner"
)

// reasoningSymbol marks a finished reasoning block: a dim diamond shown in the
// collapsed summary (the live header is an animated spinner instead).
const reasoningSymbol = "◇"

// spinnerFrames are the braille-dot frames shared by the live viewports (matching
// WithSpinner's set).
var spinnerFrames = spinner.CharSets[14]

// reasoningStream renders streaming model reasoning as an animated spinner +
// "Thinking" header above a 3-line rolling window (via promptui.StreamView),
// collapsing to "◇ thought for Ns" when finished. It is an io.Writer; copy the
// reasoning stream into it and call finish.
type reasoningStream struct {
	sv    *promptui.StreamView
	start time.Time
}

func newReasoningStream() *reasoningStream {
	return &reasoningStream{
		sv: &promptui.StreamView{
			Spinner:     spinnerFrames,
			Label:       "Thinking",
			HeaderStyle: dim,
			Window:      3,
			Indent:      "  ",
			RuneWidth:   runeWidth, // CJK-aware so Chinese reasoning wraps correctly
			Style:       dim,
			Stdout:      os.Stdout,
		},
		start: time.Now(),
	}
}

func (r *reasoningStream) Write(p []byte) (int, error) { return r.sv.Write(p) }

// finish collapses the viewport to a single "◇ thought for Ns" marker line.
func (r *reasoningStream) finish() {
	r.sv.Done(dim(fmt.Sprintf("%s thought for %s", reasoningSymbol, reasoningElapsed(r.start))))
}

// dim wraps s in the SGR faint attribute.
func dim(s string) string {
	return "\033[2m" + s + "\033[0m"
}

func reasoningElapsed(start time.Time) string {
	d := time.Since(start)
	if d < time.Second {
		return "<1s"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
