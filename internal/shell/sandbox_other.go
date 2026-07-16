//go:build !darwin && !linux

package shell

import (
	"context"
	"errors"
	"os/exec"
)

// No OS sandbox on this platform (Windows and others): bash runs unsandboxed
// and every call goes through the interactive approval gate instead.

func sandboxAvailable() bool { return false }

func sandboxCmd(context.Context, string, string, []string, bool) (*exec.Cmd, error) {
	return nil, errors.New("sandboxing is not supported on this platform")
}
