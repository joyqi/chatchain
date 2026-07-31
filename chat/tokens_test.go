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

func TestFormatTokens(t *testing.T) {
	cases := map[int]string{
		900:       "900",
		1_000:     "1k",
		12_400:    "12.4k",
		128_000:   "128k",
		1_000_000: "1m",
		1_500_000: "1.5m",
	}
	for in, want := range cases {
		if got := formatTokens(in); got != want {
			t.Errorf("formatTokens(%d) = %q, want %q", in, got, want)
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

// The live meter moves the ctx figure DURING a turn: streamed deltas batch
// into an ≈ estimate, appended messages land immediately, a settle replaces
// everything with real usage, and a snapshot restore rolls a failed turn's
// estimates back out.
func TestCtxMeterLiveFlow(t *testing.T) {
	b := &contextBudget{window: 100_000, counter: newTokenCounter(), used: 10_000, haveUsage: true}
	pushes := 0
	m := newCtxMeter(b, usageStub{in: 12_000, out: 500}, func() { pushes++ })
	m.every = 0 // no throttle in tests: every Write flushes and pushes

	snap := b.snap()
	m.Write([]byte("some streamed output tokens"))
	if b.used <= 10_000 || b.haveUsage {
		t.Fatalf("after stream delta: used=%d haveUsage=%v, want a marked estimate above the base", b.used, b.haveUsage)
	}
	m.note(provider.Message{Role: "tool", Content: "a big tool result"})
	if pushes == 0 {
		t.Fatal("the status line was never pushed")
	}

	m.settle(nil)
	if b.used != 12_500 || !b.haveUsage {
		t.Fatalf("after settle: used=%d haveUsage=%v, want the provider's real figure", b.used, b.haveUsage)
	}

	// A failed turn: estimates roll back with the messages.
	m.Write([]byte("estimates from a turn that will fail"))
	b.restore(snap)
	if b.used != 10_000 || !b.haveUsage {
		t.Fatalf("after restore: used=%d haveUsage=%v, want the snapshot", b.used, b.haveUsage)
	}
}

// A nil meter (provider without token accounting) is a no-op everywhere —
// the call sites stay unconditional.
func TestCtxMeterNilSafe(t *testing.T) {
	var m *ctxMeter
	if n, err := m.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("nil Write = (%d, %v)", n, err)
	}
	m.note(provider.Message{Content: "x"})
	m.settle(nil)
	m.reset()
}

// usageStub is a minimal UsageReporter-bearing provider for settle tests.
type usageStub struct{ in, out int }

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
func (s usageStub) LastUsage() (int, int, bool) { return s.in, s.out, true }
func (usageStub) ResetUsage()                   {}

func TestShouldOfferCompact(t *testing.T) {
	// Window 100k, threshold 80% → 80k. Snooze step 5% → 5k.
	b := &contextBudget{window: 100_000}

	// Below the threshold: never offered, declined or not.
	b.used = 70_000
	if b.shouldOfferCompact(0, 0) {
		t.Error("below threshold: offered, want not")
	}

	// At the threshold, never declined: offered.
	b.used = 80_000
	if !b.shouldOfferCompact(0, 0) {
		t.Error("at threshold, never declined: not offered, want offered")
	}

	// Declined at 80k: snoozed until usage grows by 5% of the window.
	if b.shouldOfferCompact(0, 80_000) {
		t.Error("just declined: offered, want snoozed")
	}
	b.used = 84_000
	if b.shouldOfferCompact(0, 80_000) {
		t.Error("grown <5%: offered, want snoozed")
	}
	b.used = 85_000
	if !b.shouldOfferCompact(0, 80_000) {
		t.Error("grown 5%: not offered, want offered")
	}

	// extra counts toward the growth, matching shouldCompact's accounting.
	b.used = 83_000
	if !b.shouldOfferCompact(2_000, 80_000) {
		t.Error("used+extra grown 5%: not offered, want offered")
	}
}
