package list

import (
	"reflect"
	"strings"
	"testing"
)

func TestVisibleIndices(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	t.Run("whole list fits", func(t *testing.T) {
		l, _ := New(items, 10)
		if got := l.VisibleIndices(); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4}) {
			t.Errorf("got %v", got)
		}
	})

	t.Run("scrolled window tracks absolute indices", func(t *testing.T) {
		l, _ := New(items, 2)
		if got := l.VisibleIndices(); !reflect.DeepEqual(got, []int{0, 1}) {
			t.Fatalf("initial got %v", got)
		}
		l.Next() // cursor 1, window still [0,1]
		l.Next() // cursor 2, window scrolls to [1,2]
		if got := l.VisibleIndices(); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("after scroll got %v", got)
		}
	})

	t.Run("g and G jump to first and last", func(t *testing.T) {
		l, _ := New([]string{"a", "b", "c", "d"}, 2) // window smaller than list
		if l.Len() != 4 {
			t.Fatalf("Len = %d, want 4", l.Len())
		}
		l.SetCursor(l.Len() - 1) // G -> last
		if l.Index() != 3 {
			t.Errorf("G -> Index %d, want 3", l.Index())
		}
		l.SetCursor(0) // g -> first
		if l.Index() != 0 {
			t.Errorf("g -> Index %d, want 0", l.Index())
		}
	})

	t.Run("search filtering keeps absolute indices", func(t *testing.T) {
		src := []string{"apple", "banana", "avocado", "cherry"}
		l, _ := New(src, 10)
		l.Searcher = func(term string, idx int) bool {
			return strings.HasPrefix(src[idx], term)
		}
		l.Search("a") // matches apple(0) and avocado(2)
		got := l.VisibleIndices()
		if !reflect.DeepEqual(got, []int{0, 2}) {
			t.Errorf("filtered visible indices = %v, want [0 2]", got)
		}
		// VisibleIndices must agree with Index() for the cursor row.
		if idx := l.Index(); idx != got[0] {
			t.Errorf("Index()=%d, want %d (first visible)", idx, got[0])
		}
	})
}
