package ui

import (
	"fmt"
	"os"
	"sync"
)

// regionTrace is the op-trace sink for the staging window, opened once from
// $IOTA_DEBUG_REGION (empty = disabled). Spacing faults are invisible to
// unit tests when they sit in a producer or in the renderer rather than the
// region itself — a live op trace against a real provider is the fastest way
// to localize which layer emitted a stray row (this is how the reset-only
// line from color.Fprintf was found).
var regionTrace = sync.OnceValue(func() *os.File {
	path := os.Getenv("IOTA_DEBUG_REGION")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
})

func debugRegion(format string, a ...any) {
	f := regionTrace()
	if f == nil {
		return
	}
	fmt.Fprintf(f, format+"\n", a...)
}
