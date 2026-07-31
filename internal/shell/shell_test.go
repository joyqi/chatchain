package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cancelled run must actually END. The default CommandContext kill
// reached only the DIRECT child (the sandbox wrapper or bash), and any
// surviving descendant holding the output pipe wedged Wait — ESC on a
// long tool call looked completely dead while the work kept running.
func TestRunCancelKillsTheTree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res := Run(ctx, Options{Command: "sleep 30 & sleep 30 & wait"})
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("cancelled run took %v, want a prompt return", el)
	}
	if !res.Cancelled {
		t.Fatalf("result = %+v, want Cancelled", res)
	}
}

// The group kill must reach THROUGH the sandbox wrapper: the direct child
// is sandbox-exec / bwrap, and bash lives a level below it.
func TestRunCancelKillsTheSandboxedTree(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox on this platform")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res := Run(ctx, Options{Command: "sleep 30", Sandbox: &Sandbox{Root: t.TempDir()}})
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("cancelled sandboxed run took %v, want a prompt return", el)
	}
	if !res.Cancelled {
		t.Fatalf("result = %+v, want Cancelled", res)
	}
}

// A background child that outlives bash must not wedge the run either:
// bash exits, the child still holds the inherited output pipe, and
// WaitDelay bounds the wait — the run reports as exited with whatever
// output made it through.
func TestRunBackgroundChildDoesNotWedge(t *testing.T) {
	start := time.Now()
	res := Run(context.Background(), Options{Command: "sleep 30 & echo started"})
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("run with a lingering child took %v, want the WaitDelay bound", el)
	}
	if !res.Exited || !strings.Contains(res.Output, "started") {
		t.Fatalf("result = %+v, want Exited with the foreground output", res)
	}
}

func run(t *testing.T, opts Options) Result {
	t.Helper()
	return Run(context.Background(), opts)
}

func TestRunShellSemantics(t *testing.T) {
	dir := t.TempDir()

	// Pipes, expansion, chaining — a real shell.
	res := run(t, Options{Command: "echo hello | tr a-z A-Z && x=5; echo $((x+1))", Dir: dir})
	if res.Err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "HELLO") || !strings.Contains(res.Output, "6") {
		t.Fatalf("shell semantics: %+v", res)
	}

	// Exit codes are reported, not turned into Err.
	res = run(t, Options{Command: "exit 3", Dir: dir})
	if res.Err != nil || !res.Exited || res.ExitCode != 3 {
		t.Fatalf("exit code: %+v", res)
	}

	// The working directory applies.
	res = run(t, Options{Command: "pwd", Dir: dir})
	if !strings.Contains(res.Output, filepath.Base(dir)) {
		t.Fatalf("cwd: %+v", res)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Run(ctx, Options{Command: "echo hi", Dir: t.TempDir()})
	if !res.Cancelled {
		t.Fatalf("pre-cancelled context should report Cancelled: %+v", res)
	}
}

// Sandbox isolation, exercised only where an OS sandbox actually runs (and
// skipped when the environment prevents it, e.g. nested sandboxing).
func TestSandboxIsolation(t *testing.T) {
	if !Available() {
		t.Skip("no OS sandbox on this platform")
	}
	root := t.TempDir()
	sb := &Sandbox{Root: root}
	if res := run(t, Options{Command: "echo probe", Dir: root, Sandbox: sb}); res.Err != nil || res.ExitCode != 0 {
		t.Skipf("sandbox not runnable in this environment: %+v", res)
	}

	// Writes inside the project root succeed.
	res := run(t, Options{Command: "echo data > inside.txt && cat inside.txt", Dir: root, Sandbox: sb})
	if res.ExitCode != 0 || !strings.Contains(res.Output, "data") {
		t.Fatalf("in-root write failed: %+v", res)
	}

	// Writes outside the writable roots are denied. HOME itself is outside
	// (only the cache dir under it is writable).
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	outside, err := os.MkdirTemp(home, ".chatchain-sbx-test-")
	if err != nil {
		t.Skipf("cannot create probe dir in home: %v", err)
	}
	defer os.RemoveAll(outside)
	res = run(t, Options{Command: "echo x > " + filepath.Join(outside, "f.txt"), Dir: root, Sandbox: sb})
	if res.ExitCode == 0 && res.Err == nil {
		t.Fatalf("outside-root write should be denied: %+v", res)
	}

	// An extra Write root opens exactly that directory.
	res = run(t, Options{Command: "echo y > " + filepath.Join(outside, "g.txt"), Dir: root,
		Sandbox: &Sandbox{Root: root, Write: []string{outside}}})
	if res.ExitCode != 0 || res.Err != nil {
		t.Fatalf("configured write root should be writable: %+v", res)
	}
}

func TestWritablePaths(t *testing.T) {
	root := t.TempDir()
	paths := writablePaths(&Sandbox{Root: root, Write: []string{root, "", "/tmp"}})
	if paths[0] != root {
		t.Fatalf("project root must lead: %v", paths)
	}
	seen := map[string]int{}
	for _, p := range paths {
		seen[p]++
		if seen[p] > 1 {
			t.Fatalf("duplicate writable path %q: %v", p, paths)
		}
	}
}

func TestTruncateOutput(t *testing.T) {
	// Byte cap: head and tail survive, the middle is elided.
	head := strings.Repeat("H", headBytes)
	tail := strings.Repeat("T", tailBytes)
	out := truncateOutput(head + strings.Repeat("M", 10*1024) + tail)
	if !strings.HasPrefix(out, "H") || !strings.HasSuffix(out, "T") {
		t.Fatal("head/tail not preserved")
	}
	if !strings.Contains(out, "bytes omitted") || strings.Contains(out, "M") {
		t.Fatal("middle should be dropped with a marker")
	}

	// Line cap: many short lines are elided by count, first and last kept.
	var b strings.Builder
	for i := 1; i <= 3*maxOutputLines; i++ {
		fmt.Fprintf(&b, "line-%d\n", i)
	}
	out = truncateOutput(b.String())
	if !strings.Contains(out, "lines omitted") {
		t.Fatalf("line marker missing:\n%.200s", out)
	}
	if !strings.Contains(out, "line-1\n") || !strings.Contains(out, fmt.Sprintf("line-%d", 3*maxOutputLines)) {
		t.Fatal("first/last lines not preserved")
	}
	if n := strings.Count(out, "\n"); n > maxOutputLines+2 {
		t.Fatalf("still %d lines after the cap", n)
	}

	if small := "short output"; truncateOutput(small) != small {
		t.Fatal("small output must pass through untouched")
	}
}

// cappedBuffer bounds memory while a command streams unbounded output, and
// reassembles head + tail with an omission marker.
func TestCappedBuffer(t *testing.T) {
	var b cappedBuffer
	b.Write([]byte("start-"))
	chunk := strings.Repeat("x", 8*1024)
	for i := 0; i < 40; i++ { // ~320KB through a ~52KB window
		b.Write([]byte(chunk))
	}
	b.Write([]byte("-end"))
	if cap := headBytes + 2*tailBytes + 16*1024; len(b.head)+len(b.tail) > cap {
		t.Fatalf("buffer grew to %d bytes, want ≤ %d", len(b.head)+len(b.tail), cap)
	}
	out := b.String()
	if !strings.HasPrefix(out, "start-") || !strings.HasSuffix(out, "-end") {
		t.Fatal("stream head/tail not preserved")
	}
	if !strings.Contains(out, "bytes omitted") {
		t.Fatal("missing omission marker")
	}

	// Small writes pass through exactly.
	var s cappedBuffer
	s.Write([]byte("hello "))
	s.Write([]byte("world"))
	if s.String() != "hello world" {
		t.Fatalf("small stream = %q", s.String())
	}
}

// End to end: a command spewing tens of thousands of lines comes back capped.
func TestRunOutputCapped(t *testing.T) {
	res := run(t, Options{Command: "seq 1 50000", Dir: t.TempDir()})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("seq failed: %+v", res)
	}
	if len(res.Output) > maxOutputBytes+2048 {
		t.Fatalf("output %d bytes, want ≈ ≤ %d", len(res.Output), maxOutputBytes)
	}
	if !strings.Contains(res.Output, "omitted") || !strings.Contains(res.Output, "50000") {
		t.Fatalf("capped output should keep the tail and a marker:\n%.200s", res.Output)
	}
}
