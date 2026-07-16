//go:build darwin

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// macOS sandbox: Seatbelt via /usr/bin/sandbox-exec, the same primitive
// Claude Code (sandbox-runtime) and Codex CLI build on. The profile allows
// everything by default, then denies file writes outside the writable roots
// and (optionally) all network access — in SBPL the LAST matching rule wins,
// so the targeted allows override the broad denies.

const sandboxExecPath = "/usr/bin/sandbox-exec"

func sandboxAvailable() bool {
	fi, err := os.Stat(sandboxExecPath)
	return err == nil && fi.Mode().IsRegular()
}

func sandboxCmd(ctx context.Context, bashPath, script string, writable []string, network bool) (*exec.Cmd, error) {
	// /tmp and /var are symlinks into /private on macOS; Seatbelt matches the
	// resolved path, so each writable root contributes its /private twin and
	// its fully-resolved form.
	expanded := make([]string, 0, len(writable)*2)
	for _, p := range writable {
		expanded = append(expanded, p)
		for _, prefix := range []string{"/tmp", "/var", "/etc"} {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				expanded = append(expanded, "/private"+p)
			}
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
			expanded = append(expanded, resolved)
		}
	}

	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n")
	if !network {
		profile.WriteString("(deny network*)\n")
	}
	profile.WriteString("(deny file-write*)\n(allow file-write*\n  (subpath \"/dev\")\n")
	args := []string{"-p", ""} // profile text filled in below
	for i, p := range expanded {
		// Paths go in as -D parameters, never spliced into the profile text —
		// quoting stays sandbox-exec's problem, not ours.
		fmt.Fprintf(&profile, "  (subpath (param \"W%d\"))\n", i)
		args = append(args, "-D", fmt.Sprintf("W%d=%s", i, p))
	}
	profile.WriteString(")\n")
	args[1] = profile.String()

	args = append(args, bashPath, "-c", script)
	return exec.CommandContext(ctx, sandboxExecPath, args...), nil
}
