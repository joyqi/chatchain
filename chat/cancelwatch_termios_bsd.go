//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package chat

import "golang.org/x/sys/unix"

// Termios ioctl request codes on BSD-lineage systems (macOS included).
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
