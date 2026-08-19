package chat

import (
	"context"
	"io"
	"testing"

	"chatchain/provider"
)

func TestParseWindowSize(t *testing.T) {
	ok := map[string]int{
		"128000": 128_000,
		"200k":   200_000,
		"200K":   200_000,
		"1m":     1_000_000,
		"1.5m":   1_500_000,
		"2b":     2_000_000_000,
		" 64k ":  64_000,
	}
	for in, want := range ok {
		got, err := ParseWindowSize(in)
		if err != nil {
			t.Errorf("ParseWindowSize(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWindowSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "0", "k", "1x"} {
		if _, err := ParseWindowSize(bad); err == nil {
			t.Errorf("ParseWindowSize(%q) expected error, got nil", bad)
		}
	}
}

func TestTokenCounter(t *testing.T) {
	c := newTokenCounter()
	if c.enc == nil {
		t.Fatal("tiktoken encoder failed to load offline (enc is nil)")
	}
	if n := c.count(""); n != 0 {
		t.Errorf("count(empty) = %d, want 0", n)
	}
	if n := c.count("hello world, this is a test"); n <= 0 || n > 20 {
		t.Errorf("count of short sentence = %d, want small positive", n)
	}
}

// The meter moves the figure DURING a turn: appended messages land in the
// pending estimate immediately, a settle replaces the whole thing with the
// provider's real usage, and a snapshot restore rolls a failed turn back out.
//
// pending is REPLACED on every note, never added to — the meter owns the set
// of messages it stands for, so re-noting cannot inflate it.
func TestCtxMeterLiveFlow(t *testing.T) {
	b := &contextBudget{window: 100_000, counter: newTokenCounter(), settled: 10_000, haveUsage: true}
	pushes := 0
	m := newCtxMeter(b, usageStub{in: 12_000, out: 500, total: 12_500}, func() { pushes++ })

	snap := b.snap()
	m.note(provider.Message{Role: "tool", Content: "a big tool result"})
	if b.settled != 10_000 {
		t.Fatalf("note moved the settled figure: %d", b.settled)
	}
	if b.pending <= 0 || b.used() <= 10_000 {
		t.Fatalf("pending=%d used=%d, want an estimate above the settled base", b.pending, b.used())
	}
	if pushes == 0 {
		t.Fatal("the status line was never pushed")
	}
	first := b.pending

	// A second note re-estimates the WHOLE pending set: two identical
	// messages cost exactly twice one, never more.
	m.note(provider.Message{Role: "tool", Content: "a big tool result"})
	if b.pending != 2*first {
		t.Fatalf("pending=%d, want 2x%d", b.pending, first)
	}

	m.settle(nil)
	if b.used() != 12_500 || !b.haveUsage || b.pending != 0 {
		t.Fatalf("after settle: used=%d haveUsage=%v pending=%d, want the provider figure alone",
			b.used(), b.haveUsage, b.pending)
	}

	// A failed turn: the estimate rolls back with the messages.
	m.note(provider.Message{Role: "user", Content: "a turn that will fail"})
	b.restore(snap)
	if b.used() != 10_000 || !b.haveUsage {
		t.Fatalf("after restore: used=%d haveUsage=%v, want the snapshot", b.used(), b.haveUsage)
	}
}

// reset drops the pending set at a turn boundary — residue there would be
// charged to the next turn.
func TestCtxMeterResetClearsPending(t *testing.T) {
	b := &contextBudget{window: 100_000, counter: newTokenCounter(), settled: 5_000}
	m := newCtxMeter(b, usageStub{}, func() {})
	m.note(provider.Message{Role: "user", Content: "some pending content"})
	if b.pending == 0 {
		t.Fatal("note recorded nothing")
	}
	m.reset()
	if b.pending != 0 || b.used() != 5_000 {
		t.Fatalf("after reset: pending=%d used=%d, want the settled figure alone", b.pending, b.used())
	}
}

// record books a finished call in TWO places at once — the message that call
// produced (which goes to disk) and the session totals (which drive the ↑/↓
// figures) — so a resumed session recomputes exactly what the live status
// line showed. A call that reported nothing books nothing: the previous
// call's numbers must never be counted twice.
func TestCtxMeterRecord(t *testing.T) {
	b := &contextBudget{window: 100_000, counter: newTokenCounter()}
	p := &usageStub{in: 1_200, out: 300}
	m := newCtxMeter(b, p, func() {})

	first := provider.Usage{Input: 1_200, Output: 300}
	var msg provider.Message
	if u := m.record(&msg); u == nil || *u != first {
		t.Fatalf("record returned %+v, want %+v", u, first)
	}
	if msg.Usage == nil || *msg.Usage != first {
		t.Fatalf("message not stamped: %+v", msg.Usage)
	}
	if got := m.totals(); got != first {
		t.Fatalf("totals = %+v, want %+v", got, first)
	}

	// A call the provider reported no usage for: nothing stamped, nothing added.
	p.noUsage = true
	var quiet provider.Message
	if u := m.record(&quiet); u != nil || quiet.Usage != nil {
		t.Fatalf("a usage-less call booked %+v / %+v", u, quiet.Usage)
	}
	if got := m.totals(); got != first {
		t.Fatalf("totals moved on a usage-less call: %+v", got)
	}

	// Another accounted call accumulates on top.
	p.noUsage = false
	p.in, p.out = 800, 100
	m.record(nil) // no message (a compaction pass) — still counted
	if want := (provider.Usage{Input: 2_000, Output: 400}); m.totals() != want {
		t.Fatalf("totals = %+v, want %+v", m.totals(), want)
	}

	// Seeding replaces them wholesale: resume, or a switch to another session.
	m.seedTotals(provider.Usage{Input: 9, Output: 8})
	if want := (provider.Usage{Input: 9, Output: 8}); m.totals() != want {
		t.Fatalf("after seed: %+v, want %+v", m.totals(), want)
	}
}

// A nil meter (provider without token accounting) is a no-op everywhere —
// the call sites stay unconditional.
func TestCtxMeterNilSafe(t *testing.T) {
	var m *ctxMeter
	m.note(provider.Message{Content: "x"})
	m.settle(nil)
	m.reset()
	if u := m.record(&provider.Message{}); u != nil {
		t.Fatalf("nil record = %+v", u)
	}
	if got := m.totals(); got != (provider.Usage{}) {
		t.Fatalf("nil totals = %+v", got)
	}
	m.seedTotals(provider.Usage{Input: 1})
}

// usageStub is a minimal UsageReporter-bearing provider for settle tests.
// noUsage makes it report nothing, like a call whose response carried no
// usage block.
type usageStub struct {
	in, out int
	total   int // 0 = dialect reports no total (anthropic); ContextTokens sums the parts
	noUsage bool
}

func (usageStub) ListModels(context.Context) ([]string, error) { return nil, nil }
func (usageStub) Chat(context.Context, []provider.Message) (string, error) {
	return "", nil
}
func (usageStub) StreamChat(context.Context, []provider.Message, io.Writer, io.WriteCloser) (string, string, error) {
	return "", "", nil
}
func (usageStub) Type() string                  { return "stub" }
func (usageStub) Model() string                 { return "stub" }
func (usageStub) SetModel(string)               {}
func (s usageStub) LastUsage() (int, int, bool) { return s.in, s.out, !s.noUsage }
func (s usageStub) LastUsageFull() (provider.Usage, bool) {
	return provider.Usage{Input: s.in, Output: s.out, Total: s.total}, !s.noUsage
}
func (usageStub) ResetUsage() {}

// The trigger point takes whichever rule leaves more room: the flat
// percentage on small windows, window-minus-reserve on large ones.
func TestCompactThreshold(t *testing.T) {
	for _, tc := range []struct {
		window, want int
		why          string
	}{
		{window: 20_000, want: 16_000, why: "small window: 80% (a 16k reserve would leave 4k)"},
		{window: 128_000, want: 112_000, why: "128k: reserve beats 80% (102.4k)"},
		{window: 1_000_000, want: 984_000, why: "1m: 80% would waste 184k of window"},
		{window: 16_000, want: 12_800, why: "window == reserve: percentage keeps it usable"},
	} {
		b := &contextBudget{window: tc.window}
		if got := b.threshold(); got != tc.want {
			t.Errorf("window %d: threshold = %d, want %d (%s)", tc.window, got, tc.want, tc.why)
		}
		if b.threshold() >= b.window {
			t.Errorf("window %d: threshold %d must stay below the window", tc.window, b.threshold())
		}
	}
}

func TestShouldOfferCompact(t *testing.T) {
	// Window 100k → threshold max(80k, 100k-16k) = 84k. Snooze step 5% → 5k.
	b := &contextBudget{window: 100_000}

	// Below the threshold: never offered, declined or not.
	b.settled = 70_000
	if b.shouldOfferCompact(0, 0) {
		t.Error("below threshold: offered, want not")
	}

	// At the threshold, never declined: offered.
	b.settled = 84_000
	if !b.shouldOfferCompact(0, 0) {
		t.Error("at threshold, never declined: not offered, want offered")
	}

	// Declined at 84k: snoozed until usage grows by 5% of the window.
	if b.shouldOfferCompact(0, 84_000) {
		t.Error("just declined: offered, want snoozed")
	}
	b.settled = 88_000
	if b.shouldOfferCompact(0, 84_000) {
		t.Error("grown <5%: offered, want snoozed")
	}
	b.settled = 89_000
	if !b.shouldOfferCompact(0, 84_000) {
		t.Error("grown 5%: not offered, want offered")
	}

	// extra counts toward the growth, matching shouldCompact's accounting.
	b.settled = 87_000
	if !b.shouldOfferCompact(2_000, 84_000) {
		t.Error("used+extra grown 5%: not offered, want offered")
	}
}
