//go:build unix

package shell

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// hardenProcess makes cancellation actually end the run. CommandContext's
// default Cancel kills only the DIRECT child — the sandbox wrapper
// (sandbox-exec, bwrap) or bash itself — while the real work lives further
// down the tree. Worse, any surviving descendant inherits the output pipes,
// and Wait blocks until they hit EOF: ESC looked completely dead while a
// 10-minute build kept running. A dedicated process group lets Cancel kill
// every descendant at once, and WaitDelay unwedges Wait when an escapee
// (setsid) still holds a pipe open.
func hardenProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone // the group is already gone: not an error
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
}
