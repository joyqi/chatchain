package timefmt

import (
	"testing"
	"time"
)

func TestElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "<1s"},
		{999 * time.Millisecond, "<1s"},
		{time.Second, "1s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m 0s"},
		{65 * time.Second, "1m 5s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{time.Hour, "1h 0m 0s"},
		{time.Hour + time.Minute + time.Second, "1h 1m 1s"},
		{2*time.Hour + 5*time.Minute + 30*time.Second, "2h 5m 30s"},
	}
	for _, tc := range cases {
		if got := Elapsed(tc.d); got != tc.want {
			t.Errorf("Elapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
