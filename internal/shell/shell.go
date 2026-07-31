// Package shell executes bash command lines for the shell toolset, inside an
// OS sandbox where the platform provides one (docs/design/shell-toolset.md).
// The sandbox follows the industry pattern (Claude Code sandbox-runtime,
// Codex CLI): Seatbelt via sandbox-exec on macOS, bubblewrap on Linux — file
// writes confined to the project root plus temp/cache directories, network
// blocked unless opened. This package is mechanism only: policy (approval,
// configuration, model-facing wording) lives in the tool layer.
package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Output caps, sized to the industry norms (Claude Code clips bash output at
// 30k characters, Codex CLI clips by lines and bytes): bytes bound the token
// cost — the context guard — while the line cap keeps pathological many-short-
// lines output (a build log, a huge ls) readable. Both elide the middle: test
// and compiler failures sit at the end, context at the start.
const (
	maxOutputBytes = 32 * 1024 // combined output cap per run
	headBytes      = 8 * 1024  // kept from the start when byte-truncating
	tailBytes      = 22 * 1024 // kept from the end (errors usually live there)

	maxOutputLines = 512 // line cap, applied before the byte cap
	headLines      = 128
	tailLines      = 384
)

// Sandbox describes the isolation for one run.
type Sandbox struct {
	Root    string   // project root: always writable
	Network bool     // allow outbound network inside the sandbox
	Write   []string // extra writable roots beyond root/temp/cache
}

// Options describe one bash invocation.
type Options struct {
	Command string        // bash command line (run via bash -c)
	Dir     string        // working directory
	Timeout time.Duration // hard cap; 0 means no extra timeout
	Sandbox *Sandbox      // nil = run unsandboxed
}

// Result reports one run. A non-nil Err means the run could not be performed
// (bash missing, sandbox setup failure, spawn error); a non-zero ExitCode
// with Exited=true is a normal command failure.
type Result struct {
	Output    string // combined stdout+stderr, middle-truncated
	ExitCode  int
	Exited    bool // the command ran to completion (successfully or not)
	TimedOut  bool
	Cancelled bool
	Err       error
}

// Available reports whether this platform can sandbox (sandbox-exec on
// macOS, bwrap on Linux).
func Available() bool { return sandboxAvailable() }

// Run executes opts.Command through bash.
func Run(ctx context.Context, opts Options) Result {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return Result{Err: errors.New("bash is not installed on this system")}
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	if sb := opts.Sandbox; sb != nil {
		cmd, err = sandboxCmd(ctx, bashPath, opts.Command, writablePaths(sb), sb.Network)
		if err != nil {
			return Result{Err: fmt.Errorf("failed to prepare the sandbox: %w", err)}
		}
	} else {
		cmd = exec.CommandContext(ctx, bashPath, "-c", opts.Command)
	}
	cmd.Dir = opts.Dir
	hardenProcess(cmd) // cancellation kills the TREE, and Wait cannot wedge

	var buf cappedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	res := Result{Output: truncateOutput(buf.String())}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
	case errors.Is(ctx.Err(), context.Canceled):
		res.Cancelled = true
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The command exited but a background child kept the output pipe
		// open past WaitDelay: the run is complete — the straggler's future
		// output is forfeit, which beats wedging the chat loop forever.
		res.Exited = true
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		}
	case runErr == nil:
		res.Exited = true
	default:
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			res.Exited = true
			res.ExitCode = exit.ExitCode()
		} else {
			res.Err = runErr
		}
	}
	return res
}

// writablePaths derives the sandbox-writable roots: the project root, the
// temp directories, the user cache (build caches live there — Go, npm, pip
// all break without it), and the configured extras. Deduped, in order.
func writablePaths(sb *Sandbox) []string {
	paths := []string{sb.Root, os.TempDir(), "/tmp"}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		_ = os.MkdirAll(cache, 0o755) // must exist to be bind-mounted / allowed
		paths = append(paths, cache)
	}
	for _, p := range sb.Write {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			paths = append(paths, abs)
		}
	}
	seen := make(map[string]bool, len(paths))
	out := paths[:0]
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// cappedBuffer keeps the first headBytes and the rolling last ~tailBytes of
// everything written, so a command printing gigabytes never grows memory
// beyond a few output caps. String() reassembles, marking dropped bytes.
type cappedBuffer struct {
	head  []byte
	tail  []byte
	total int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	if room := headBytes - len(b.head); room > 0 {
		take := room
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > 2*tailBytes {
			b.tail = append(b.tail[:0:0], b.tail[len(b.tail)-tailBytes:]...)
		}
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	if b.total <= len(b.head)+len(b.tail) {
		return string(b.head) + string(b.tail) // nothing was dropped
	}
	head := b.head
	for len(head) > 0 && !isRuneStart(head[len(head)-1]) {
		head = head[:len(head)-1]
	}
	tail := b.tail
	if len(tail) > tailBytes {
		tail = tail[len(tail)-tailBytes:]
	}
	for len(tail) > 0 && !isRuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := b.total - len(head) - len(tail)
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", head, omitted, tail)
}

// truncateOutput applies the line cap, then the byte cap. Markers tell the
// model what was dropped and how to narrow the output.
func truncateOutput(s string) string {
	if n := strings.Count(s, "\n") + 1; n > maxOutputLines {
		lines := strings.Split(s, "\n")
		omitted := len(lines) - headLines - tailLines
		s = strings.Join(lines[:headLines], "\n") +
			fmt.Sprintf("\n[... %d lines omitted — pipe through head/tail/grep to narrow the output ...]\n", omitted) +
			strings.Join(lines[len(lines)-tailLines:], "\n")
	}
	return truncateMiddle(s)
}

// truncateMiddle keeps the head and tail of oversized output.
func truncateMiddle(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	head := s[:headBytes]
	for len(head) > 0 && !isRuneStart(head[len(head)-1]) {
		head = head[:len(head)-1]
	}
	tail := s[len(s)-tailBytes:]
	for len(tail) > 0 && !isRuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := len(s) - len(head) - len(tail)
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", head, omitted, tail)
}

// isRuneStart reports whether b can begin a UTF-8 sequence (i.e. is not a
// continuation byte).
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
