//go:build unix && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package chat

import "golang.org/x/sys/unix"

// Termios ioctl request codes on non-BSD unix systems (Linux, Solaris, AIX).
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
