// Package timefmt renders elapsed durations for the UI. One implementation
// so every timer — thinking markers, tool clocks, the busy status bar —
// carries into minutes and hours the same way.
package timefmt

import (
	"fmt"
	"time"
)

// Elapsed renders an elapsed duration in the UI's compact style: "<1s" below
// one second, whole seconds under a minute ("45s"), then space-separated
// carried units with seconds always kept ("3m 45s", "1h 3m 45s") — a timer
// must not degrade to minute granularity just because it ran long.
func Elapsed(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	s := int(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %dm %ds", s/3600, s%3600/60, s%60)
	}
}
