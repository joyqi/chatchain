package chat

import (
	"fmt"
	"time"
)

// reasoningSymbol marks a finished reasoning block: a dim diamond shown in the
// collapsed summary (the live header is an animated spinner instead).
const reasoningSymbol = "◇"

func dim(s string) string {
	return DimStyle.Sprint(s)
}

func reasoningElapsed(start time.Time) string {
	d := time.Since(start)
	if d < time.Second {
		return "<1s"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
