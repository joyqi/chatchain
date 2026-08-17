package tokfmt

import "testing"

// The union of what the three former copies each pinned separately.
func TestTokens(t *testing.T) {
	for n, want := range map[int]string{
		0:         "0",
		842:       "842",
		900:       "900",
		1_000:     "1k",
		1_234:     "1.2k",
		12_400:    "12.4k",
		56_400:    "56.4k",
		128_000:   "128k",
		1_000_000: "1m",
		1_100_000: "1.1m",
		1_500_000: "1.5m",
	} {
		if got := Tokens(n); got != want {
			t.Errorf("Tokens(%d) = %q, want %q", n, got, want)
		}
	}
}
