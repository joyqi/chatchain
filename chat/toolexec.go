package chat

import (
	"context"
	"fmt"
	"os"
	"time"

	"chatchain/provider"
	"chatchain/tool"

	"github.com/briandowns/spinner"
	"golang.org/x/term"
)

// callTool executes a single tool call through the dispatcher. In interactive
// mode on a TTY it shows a live elapsed-time counter on the spinner and lets the
// user press ESC (or Ctrl+C) to cancel the running tool — cancellation
// propagates via ctx (e.g. killing run_command's child process). In quiet mode
// or when stdin is not a terminal it simply delegates. baseSuffix is the spinner
// label shown alongside the timer.
func callTool(ctx context.Context, dispatch tool.Dispatcher, tc provider.ToolCall,
	s *spinner.Spinner, baseSuffix string, quiet bool) (string, bool, error) {

	if quiet || !term.IsTerminal(int(os.Stdin.Fd())) {
		return dispatch.CallTool(ctx, tc.Name, tc.Arguments)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Platform hook: raw-mode stdin watch for ESC/Ctrl+C → cancel. The returned
	// func restores the terminal and stops the watcher. No-op on Windows (timer
	// still shows; ESC-to-cancel unavailable there).
	stopWatch := startCancelWatch(cancel)
	defer stopWatch()

	// Live elapsed time on the spinner suffix, refreshed each second. The write
	// races the spinner's render goroutine, so guard it with the spinner's own
	// lock (it unlocks before sleeping, so this only blocks for a frame).
	start := time.Now()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			suffix := fmt.Sprintf(" %s  %s (ESC to cancel)", baseSuffix, time.Since(start).Round(time.Second))
			s.Lock()
			s.Suffix = suffix
			s.Unlock()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()

	text, isErr, err := dispatch.CallTool(ctx, tc.Name, tc.Arguments)
	close(done)
	return text, isErr, err
}
