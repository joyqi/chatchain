//go:build windows

package shell

import (
	"os/exec"
	"time"
)

// hardenProcess (windows): no POSIX process groups to kill — keep the
// default Cancel (kill the direct child) and rely on WaitDelay to unwedge
// Wait when a descendant still holds the output pipe open.
func hardenProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
}
