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
