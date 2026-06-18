//go:build !unix

package chat

import "context"

// startCancelWatch is a no-op on platforms without poll-based stdin cancellation
// (e.g. Windows): the elapsed-time counter still shows during a tool call, but
// ESC-to-cancel is unavailable.
func startCancelWatch(cancel context.CancelFunc) func() { return func() {} }
