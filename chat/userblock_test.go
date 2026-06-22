package chat

import (
	"reflect"
	"testing"
)

func TestEchoRows(t *testing.T) {
	// userPrompt "❯ " has display width 2. The first row starts at column 2;
	// wrapped continuation rows resume flush-left at column 0.
	tests := []struct {
		name string
		raw  string
		tw   int
		want int
	}{
		{"empty", "", 80, 1},
		{"short", "hello", 80, 1},
		{"zero-width-guard", "anything", 0, 1},
		// tw=4: row0 = prompt(2)+"ab", then "cdef" (4), then "gh" — 3 rows.
		{"wrapped", "abcdefgh", 4, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := echoRows(tt.raw, tt.tw); got != tt.want {
				t.Errorf("echoRows(%q, %d) = %d, want %d", tt.raw, tt.tw, got, tt.want)
			}
		})
	}
}

func TestWrapByWidth(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"empty", "", 10, []string{""}},
		{"fits", "hello", 10, []string{"hello"}},
		{"exact", "hello", 5, []string{"hello"}},
		{"hardwrap", "abcdefg", 3, []string{"abc", "def", "g"}},
		// CJK runes are width 2: at width 3 only one full-width rune fits per row,
		// and a wide rune is never split across the boundary.
		{"cjk", "你好吗", 3, []string{"你", "好", "吗"}},
		{"cjk-mixed", "a你b", 3, []string{"a你", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrapByWidth(tt.in, tt.width); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("wrapByWidth(%q, %d) = %#v, want %#v", tt.in, tt.width, got, tt.want)
			}
		})
	}
}
