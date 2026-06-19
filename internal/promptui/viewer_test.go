package promptui

import (
	"reflect"
	"testing"
)

func stripANSI(s string) string {
	var b []byte
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			i += n
			continue
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}

func TestWrapLine(t *testing.T) {
	if got := wrapLine("0123456789", 4); !reflect.DeepEqual(got, []string{"0123", "4567", "89"}) {
		t.Errorf("plain wrap = %q", got)
	}
	if got := wrapLine("", 4); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("empty = %q", got)
	}
	if got := wrapLine("ab", 4); !reflect.DeepEqual(got, []string{"ab"}) {
		t.Errorf("fits = %q", got)
	}

	// ANSI: each row stays within the width and the visible text round-trips.
	rows := wrapLine("\x1b[1mhello\x1b[0m world!!", 5) // visible "hello world!!"
	var vis string
	for _, r := range rows {
		if w := visibleWidth(r); w > 5 {
			t.Errorf("row %q visible width %d > 5", r, w)
		}
		vis += stripANSI(r)
	}
	if vis != "hello world!!" {
		t.Errorf("reassembled = %q, want %q", vis, "hello world!!")
	}
}

func TestVisibleWidth(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"", 0},
		{"\x1b[1mhi\x1b[0m", 2},        // ANSI skipped
		{"\x1b[36m▸\x1b[0m ok", 4},     // "▸ ok" = 4 visible
		{"\x1b[1m\x1b[31mx\x1b[0m", 1}, // stacked codes
	}
	for _, tt := range tests {
		if got := visibleWidth(tt.in); got != tt.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestClipLine(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		start, width int
		want         string
	}{
		{"head", "hello world", 0, 5, "hello"},
		{"offset", "hello world", 6, 5, "world"},
		{"width zero", "hello", 0, 0, ""},
		{"past end", "hi", 5, 10, ""},
		// ANSI codes are always emitted; only visible runes are clipped.
		{"ansi preserved at head", "\x1b[1mhello\x1b[0m", 0, 3, "\x1b[1mhel\x1b[0m"},
		{"ansi preserved with offset", "\x1b[1mhello\x1b[0m", 2, 2, "\x1b[1mll\x1b[0m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clipLine(tt.in, tt.start, tt.width); got != tt.want {
				t.Errorf("clipLine(%q, %d, %d) = %q, want %q", tt.in, tt.start, tt.width, got, tt.want)
			}
		})
	}
}
