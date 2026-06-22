package promptui

import (
	"reflect"
	"testing"
	"unicode"
)

// cjkWidth is a CJK-aware width func for tests (Han/Hangul/Kana = 2 cols).
func cjkWidth(r rune) int {
	if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
		return 2
	}
	return 1
}

// newTestStreamView builds a view with a fixed wrap width that skips terminal
// detection, so the wrapping/feed logic can be inspected directly.
func newTestStreamView(width int) *StreamView {
	return &StreamView{Window: 3, RuneWidth: cjkWidth, width: width, started: true, tty: true}
}

func TestStreamViewWrap(t *testing.T) {
	v := newTestStreamView(5)
	v.feed([]byte("abcdefghij")) // width 5 -> "abcde" | "fghij"
	if want := []string{"abcde"}; !reflect.DeepEqual(v.lines, want) {
		t.Fatalf("lines = %q, want %q", v.lines, want)
	}
	if string(v.cur) != "fghij" {
		t.Fatalf("cur = %q, want %q", string(v.cur), "fghij")
	}
}

func TestStreamViewNewline(t *testing.T) {
	v := newTestStreamView(80)
	v.feed([]byte("first\nsecond"))
	if want := []string{"first"}; !reflect.DeepEqual(v.lines, want) {
		t.Fatalf("lines = %q, want %q", v.lines, want)
	}
	if string(v.cur) != "second" {
		t.Fatalf("cur = %q, want %q", string(v.cur), "second")
	}
}

func TestStreamViewCJKWidth(t *testing.T) {
	// CJK runes are width 2; at width 5 only two fit per row: 你好 | 吗世 | 界.
	v := newTestStreamView(5)
	v.feed([]byte("你好吗世界"))
	if want := []string{"你好", "吗世"}; !reflect.DeepEqual(v.lines, want) {
		t.Fatalf("lines = %q, want %q", v.lines, want)
	}
	if string(v.cur) != "界" {
		t.Fatalf("cur = %q, want %q", string(v.cur), "界")
	}
}

func TestStreamViewPartialRune(t *testing.T) {
	// A multibyte rune split across two feeds must not corrupt; "好" is 3 bytes.
	v := newTestStreamView(80)
	b := []byte("好")
	v.feed(b[:1])
	v.feed(b[1:])
	if string(v.cur) != "好" {
		t.Fatalf("cur = %q, want %q", string(v.cur), "好")
	}
	if len(v.pend) != 0 {
		t.Fatalf("pend not drained: %v", v.pend)
	}
}

func TestStreamViewVisibleWindow(t *testing.T) {
	// Only the last Window (3) completed lines plus the in-progress line show.
	v := newTestStreamView(3)
	v.feed([]byte("aaabbbcccdddee")) // aaa|bbb|ccc|ddd|ee(cur)
	want := []string{"ccc", "ddd", "ee"}
	if got := v.visibleLines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("visibleLines = %q, want %q", got, want)
	}
}
