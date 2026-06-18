//go:build unix

package chat

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// startCancelWatch puts stdin in raw mode and watches the keyboard, calling
// cancel when the user presses ESC (0x1b) or Ctrl+C (0x03). The returned func
// stops the watcher and restores the terminal. If raw mode can't be entered it
// degrades to a no-op.
//
// The watcher polls stdin with a short timeout so it exits promptly when the
// stop func runs, never leaving a blocked Read on the descriptor that could
// steal input from the next prompt. run_command's child process does not read
// the terminal (its stdin is the tool's stdin arg or /dev/null), so there is no
// contention for keystrokes.
func startCancelWatch(cancel context.CancelFunc) func() {
	fd := int(os.Stdin.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 16)
		for {
			select {
			case <-stop:
				return
			default:
			}
			pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, perr := unix.Poll(pfd, 200) // 200ms; re-check stop between polls
			if perr != nil {
				if perr == unix.EINTR {
					continue
				}
				return
			}
			if n == 0 {
				continue
			}
			m, rerr := unix.Read(fd, buf)
			if rerr != nil {
				if rerr == unix.EINTR {
					continue
				}
				return
			}
			for i := 0; i < m; i++ {
				if buf[i] == 0x1b || buf[i] == 0x03 {
					cancel()
					return
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
			_ = term.Restore(fd, st)
		})
	}
}
