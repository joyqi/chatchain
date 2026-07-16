//go:build linux

package shell

import (
	"context"
	"os"
	"os/exec"
)

// Linux sandbox: bubblewrap (bwrap), the primitive both Claude Code and
// Codex CLI use on Linux. The whole filesystem is re-mounted read-only, the
// writable roots are bind-mounted read-write on top, and the network
// namespace is unshared unless network access is enabled. Requires bwrap on
// PATH (unprivileged user namespaces); absent that, the shell set degrades
// to unsandboxed execution behind the approval gate.

func sandboxAvailable() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

func sandboxCmd(ctx context.Context, bashPath, script string, writable []string, network bool) (*exec.Cmd, error) {
	args := []string{
		"--ro-bind", "/", "/",
		"--dev-bind", "/dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
	}
	for _, p := range writable {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			args = append(args, "--bind", p, p)
		}
	}
	if !network {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--", bashPath, "-c", script)
	return exec.CommandContext(ctx, "bwrap", args...), nil
}
