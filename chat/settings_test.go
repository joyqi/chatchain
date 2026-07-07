package chat

import (
	"sort"
	"testing"
)

func TestContextWindowRowsPresetCurrent(t *testing.T) {
	values, labels, cur := contextWindowRows(128_000)
	if len(values) != len(contextPresets) {
		t.Fatalf("preset current must not grow the list: got %d rows", len(values))
	}
	if values[cur] != 128_000 || labels[cur] != "128k (current)" {
		t.Errorf("current row wrong: values[%d]=%d label=%q", cur, values[cur], labels[cur])
	}
}

func TestContextWindowRowsInsertsNonPresetSorted(t *testing.T) {
	values, labels, cur := contextWindowRows(64_000)
	if len(values) != len(contextPresets)+1 {
		t.Fatalf("non-preset current must be inserted: got %d rows", len(values))
	}
	if !sort.IntsAreSorted(values) {
		t.Errorf("values not sorted: %v", values)
	}
	if values[cur] != 64_000 || labels[cur] != "64k (current)" {
		t.Errorf("current row wrong: values[%d]=%d label=%q", cur, values[cur], labels[cur])
	}
}

func TestEffortRows(t *testing.T) {
	// The unset ("") level maps to the "default" row.
	values, labels, cur := effortRows("")
	if len(values) != len(effortLevels) || values[cur] != "" || labels[cur] != "default (current)" {
		t.Errorf("default row wrong: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}

	// A known level is marked in place.
	values, labels, cur = effortRows("high")
	if len(values) != len(effortLevels) || values[cur] != "high" || labels[cur] != "high (current)" {
		t.Errorf("known level wrong: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}

	// An unknown level (e.g. from a newer session) is appended so an untouched
	// tab stays a no-op.
	values, labels, cur = effortRows("turbo")
	if cur != len(effortLevels) || values[cur] != "turbo" || labels[cur] != "turbo (current)" {
		t.Errorf("unknown level not appended: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}
}

func TestModelRows(t *testing.T) {
	models := []string{"a-model", "b-model"}

	// Current model present in the list: marked in place, no growth.
	values, labels, cur := modelRows("b-model", models)
	if len(values) != 2 || values[cur] != "b-model" || labels[cur] != "b-model (current)" {
		t.Errorf("present current wrong: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}

	// Current model missing from the list (delisted model, different naming
	// scheme): prepended so an untouched tab stays a no-op.
	values, labels, cur = modelRows("models/c", models)
	if cur != 0 || values[0] != "models/c" || labels[0] != "models/c (current)" {
		t.Errorf("missing current not prepended: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}
	if len(values) != 3 {
		t.Errorf("missing current must grow the list: got %d rows", len(values))
	}

	// No model selected yet: a "(not selected)" row representing "" is
	// prepended; committing it is a no-op (guarded by the "" check).
	values, labels, cur = modelRows("", models)
	if cur != 0 || values[0] != "" || labels[0] != "(not selected)" {
		t.Errorf("empty current wrong: values[%d]=%q label=%q", cur, values[cur], labels[cur])
	}
}

func TestFloatPtrEqual(t *testing.T) {
	a, b, c := 0.7, 0.7, 0.8
	cases := []struct {
		x, y *float64
		want bool
	}{
		{nil, nil, true},
		{&a, nil, false},
		{nil, &a, false},
		{&a, &b, true},
		{&a, &c, false},
	}
	for i, tc := range cases {
		if got := floatPtrEqual(tc.x, tc.y); got != tc.want {
			t.Errorf("case %d: floatPtrEqual = %v, want %v", i, got, tc.want)
		}
	}
}
