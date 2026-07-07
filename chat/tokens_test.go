package chat

import "testing"

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
