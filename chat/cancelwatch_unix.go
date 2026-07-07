//go:build unix

package chat

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// makeRawInput mirrors x/term's MakeRaw except it leaves the output flags
// (OPOST in particular) untouched: raw input — byte-at-a-time, no echo, no
// signal keys — with normal output processing, so streamed output (the
// markdown render, the reasoning viewport) keeps rendering while a watch is
// active. Same trade-off as the vendored readline term package, which also
// keeps OPOST. Returns the previous state for restore.
func makeRawInput(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	saved := *old

	t := *old
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &t); err != nil {
		return nil, err
	}
	return &saved, nil
}

// startCancelWatch puts stdin in raw mode (input only; output processing stays
// on) and watches the keyboard, calling cancel when the user presses ESC (a
// lone 0x1b — escape sequences such as arrow keys, paste markers, or terminal
// replies also start with 0x1b and are dropped, not misread as ESC) or Ctrl+C
// (0x03). The returned func stops the watcher and restores the terminal. If
// raw mode can't be entered it degrades to a no-op.
//
// The watcher polls stdin with a short timeout so it exits promptly when the
// stop func runs, never leaving a blocked Read on the descriptor that could
// steal input from the next prompt. run_command's child process does not read
// the terminal (its stdin is the tool's stdin arg or /dev/null), so there is no
// contention for keystrokes.
func startCancelWatch(cancel context.CancelFunc) func() {
	fd := int(os.Stdin.Fd())
	st, err := makeRawInput(fd)
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
			// Ctrl+C anywhere in the chunk cancels immediately.
			for i := 0; i < m; i++ {
				if buf[i] == 0x03 {
					cancel()
					return
				}
			}
			// ESC cancels only as a lone keypress. Bytes following 0x1b in
			// the same read mean an escape sequence (arrow key, paste marker,
			// terminal reply) — the chunk is dropped. A trailing 0x1b is
			// ambiguous (lone ESC vs a split sequence), so peek briefly for a
			// continuation, the same disambiguation the vendored readline
			// uses for its lone-ESC handling.
			if m > 0 && buf[m-1] == 0x1b {
				pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				if n, perr := unix.Poll(pfd, 50); perr == nil && n > 0 {
					_, _ = unix.Read(fd, buf) // sequence tail — drain and drop
					continue
				}
				cancel()
				return
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
			_ = unix.IoctlSetTermios(fd, ioctlWriteTermios, st)
		})
	}
}
