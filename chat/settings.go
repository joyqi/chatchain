package chat

import (
	"sort"
	"strconv"
)

// effortLevels are the /model Effort tab's choices; "" is the "default" row,
// meaning the parameter is omitted from requests entirely. Levels are passed to
// the provider verbatim — a value the model doesn't support fails visibly at
// the API and the user picks another (see docs/design/model-settings.md).
var effortLevels = []string{"", "low", "medium", "high", "xhigh", "max"}

// contextPresets are the /model Context tab's stock window sizes.
var contextPresets = []int{8_000, 32_000, 128_000, 200_000, 256_000, 1_000_000}

// effortLabel renders an effort level for display ("" → "default").
func effortLabel(level string) string {
	if level == "" {
		return "default"
	}
	return level
}

// formatTemperature renders an optional temperature for display (nil →
// "default", otherwise the fewest digits that round-trip).
func formatTemperature(v *float64) string {
	if v == nil {
		return "default"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// floatPtrEqual reports whether two optional floats hold the same value (both
// nil, or both set and equal).
func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// contextWindowRows builds the Context tab's rows: the preset windows plus the
// current one (inserted sorted when it isn't a preset), the current row marked.
// Returns the row values, their labels, and the current row's index.
func contextWindowRows(current int) (values []int, labels []string, curIdx int) {
	values = append(values, contextPresets...)
	found := false
	for _, v := range values {
		if v == current {
			found = true
			break
		}
	}
	if !found && current > 0 {
		values = append(values, current)
		sort.Ints(values)
	}
	labels = make([]string, len(values))
	for i, v := range values {
		labels[i] = formatTokens(v)
		if v == current {
			labels[i] += " (current)"
			curIdx = i
		}
	}
	return values, labels, curIdx
}

// effortRows builds the Effort tab's rows: the known levels plus the current
// one (appended when unknown, so an untouched tab stays a no-op), the current
// row marked. Returns the row values ("" = default), their labels, and the
// current row's index.
func effortRows(current string) (values []string, labels []string, curIdx int) {
	values = append(values, effortLevels...)
	found := false
	for _, v := range values {
		if v == current {
			found = true
			break
		}
	}
	if !found {
		values = append(values, current)
	}
	labels = make([]string, len(values))
	for i, v := range values {
		labels[i] = effortLabel(v)
		if v == current {
			labels[i] += " (current)"
			curIdx = i
		}
	}
	return values, labels, curIdx
}

// modelRows builds the Model tab's rows: the fetched models plus the current
// one (prepended when missing, so an untouched tab stays a no-op — e.g. a
// resumed session whose model the provider no longer lists, or a list that
// uses a different naming scheme). When no model is selected yet a
// "(not selected)" row representing "" is prepended for the same reason.
// Returns the row values, their labels, and the current row's index.
func modelRows(current string, models []string) (values []string, labels []string, curIdx int) {
	values = append(values, models...)
	found := false
	for _, v := range values {
		if v == current {
			found = true
			break
		}
	}
	if !found {
		values = append([]string{current}, values...)
	}
	labels = make([]string, len(values))
	for i, v := range values {
		switch {
		case v == "":
			labels[i] = "(not selected)"
		case v == current:
			labels[i] = v + " (current)"
		default:
			labels[i] = v
		}
		if v == current {
			curIdx = i
		}
	}
	return values, labels, curIdx
}

// choiceRows builds a List tab over an optional string parameter: row 0 is
// "default" (omit the parameter), the offered options follow, and a
// configured value outside the list is appended so an untouched tab stays a
// no-op (the modelRows convention). The current row is labeled "(current)".
func choiceRows(current string, options []string) (values []string, labels []string, curIdx int) {
	values = append(values, "")
	for _, o := range options {
		values = append(values, o)
	}
	found := false
	for _, v := range values {
		if v == current {
			found = true
			break
		}
	}
	if !found {
		values = append(values, current)
	}
	labels = make([]string, len(values))
	for i, v := range values {
		if v == "" {
			labels[i] = "default"
		} else {
			labels[i] = v
		}
		if v == current {
			labels[i] += " (current)"
			curIdx = i
		}
	}
	return values, labels, curIdx
}
