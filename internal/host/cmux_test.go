package host

import (
	"errors"
	"strings"
	"testing"
)

// TestCmuxBatches pins the CLI sequence per state — icon and color included:
// they are what gives the sidebar row display parity with the built-in
// agents (a bare set-status renders no icon at all).
func TestCmuxBatches(t *testing.T) {
	join := func(batch [][]string) string {
		lines := make([]string, len(batch))
		for i, argv := range batch {
			lines[i] = strings.Join(argv, " ")
		}
		return strings.Join(lines, "\n")
	}
	for state, want := range map[State]string{
		StateBusy:       "set-status iota Running --icon bolt.fill --color #4C8DFF\nworkspace loading on --id iota",
		StateNeedsInput: "workspace loading off --id iota\nset-status iota Needs input --icon bell.fill",
		StateError:      "workspace loading off --id iota\nset-status iota Failed --icon exclamationmark.triangle.fill --color #FF3B30",
		StateIdle:       "workspace loading off --id iota\nset-status iota Idle --icon pause.circle.fill --color #8E8E93",
	} {
		if got := join(cmuxBatch(state)); got != want {
			t.Errorf("state %v:\n got %q\nwant %q", state, got, want)
		}
	}
}

// TestCmuxCoalescing: the mailbox is last-wins — states posted while the
// worker is busy collapse to the newest, so a stale "Needs input" can never
// land after the turn moved on. Close flushes the clear sequence.
func TestCmuxCoalescing(t *testing.T) {
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var got [][]string
	c := newCmux(func(argv []string) {
		entered <- struct{}{}
		<-release
		got = append(got, argv)
	})

	c.SetState(StateBusy)
	<-entered // the worker is inside the busy batch's first command

	c.SetState(StateNeedsInput)
	c.SetState(StateIdle) // replaces NeedsInput in the mailbox

	release <- struct{}{} // busy cmd 1 completes
	for i := 0; i < 3; i++ {
		<-entered
		release <- struct{}{} // busy cmd 2, then the idle batch's two commands
	}

	go c.Close()
	for i := 0; i < 2; i++ {
		<-entered
		release <- struct{}{} // the close batch: loading off + clear-status
	}
	// got is safe to read: Close waited for the worker before returning...
	// except we ran Close concurrently; sync on its completion instead.
	<-c.done

	var cmds []string
	for _, argv := range got {
		cmds = append(cmds, argv[0]+" "+argv[1])
	}
	want := []string{
		"set-status iota", "workspace loading", // busy
		"workspace loading", "set-status iota", // idle (needs-input dropped)
		"workspace loading", "clear-status iota", // close
	}
	if strings.Join(cmds, ",") != strings.Join(want, ",") {
		t.Fatalf("executed = %v\nwant heads %v", cmds, want)
	}
	for _, argv := range got {
		if strings.Contains(strings.Join(argv, " "), "Needs input") {
			t.Fatal("the superseded Needs-input batch still executed")
		}
	}
}

// TestDetectCmux: both markers must be present — the env var cmux injects
// into every pane, and its CLI on PATH.
func TestDetectCmux(t *testing.T) {
	withVar := func(k string) string {
		if k == "CMUX_SURFACE_ID" {
			return "surface-1"
		}
		return ""
	}
	if h := detectCmux(Env{Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "/usr/bin/true", nil }}); h != nil {
		t.Error("detected cmux without CMUX_SURFACE_ID")
	}
	if h := detectCmux(Env{Getenv: withVar, LookPath: func(string) (string, error) { return "", errors.New("no cmux") }}); h != nil {
		t.Error("detected cmux without the CLI on PATH")
	}
	h := detectCmux(Env{Getenv: withVar, LookPath: func(string) (string, error) { return "/usr/bin/true", nil }})
	if h == nil || h.Name() != "cmux" {
		t.Fatalf("cmux not detected: %v", h)
	}
	h.(*Cmux).Close()
}
