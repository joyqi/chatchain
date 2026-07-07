package chat

import (
	"io"
	"sort"
	"strconv"

	"chatchain/internal/promptui"
	"chatchain/provider"
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

// manageModelSettings opens the four-tab /model questionnaire — Model, Context,
// Effort, Temperature — each tab pre-seeded with the session's current value.
// Enter commits every tab regardless of focus (untouched tabs are no-ops); each
// knob that actually changed is applied to the provider/budget, persisted to
// the session, and reported on its own line. Cancel (Esc/q/Ctrl+C) changes
// nothing and prints nothing.
func manageModelSettings(w io.Writer, p provider.Provider, budget *contextBudget, sw *SessionWriter, models []string) {
	modelValues, modelLabels, modelIdx := modelRows(p.Model(), models)
	modelPanel := promptui.NewListPanel("Model", modelLabels, false)
	modelPanel.RuneWidth = runeWidth
	modelPanel.SetCursor(modelIdx)

	windows, windowLabels, windowIdx := contextWindowRows(budget.window)
	contextPanel := promptui.NewListPanel("Context", windowLabels, false)
	contextPanel.RuneWidth = runeWidth
	contextPanel.SetCursor(windowIdx)

	tun, tunable := p.(provider.Tunable)
	curEffort := ""
	var curTemp *float64
	if tunable {
		curEffort = tun.Effort()
		curTemp = tun.Temperature()
	}
	levels, levelLabels, levelIdx := effortRows(curEffort)
	effortPanel := promptui.NewListPanel("Effort", levelLabels, false)
	effortPanel.RuneWidth = runeWidth
	effortPanel.SetCursor(levelIdx)

	// Anthropic caps temperature at 1.0; the other providers allow up to 2.0.
	maxTemp := 2.0
	if p.Type() == "anthropic" {
		maxTemp = 1.0
	}
	tempPanel := promptui.NewSliderPanel("Temperature", 0, maxTemp, 0.1)
	tempPanel.RuneWidth = runeWidth
	tempPanel.SetValue(curTemp)

	tb := &promptui.Tabbed{
		Panels:    []promptui.Panel{modelPanel, contextPanel, effortPanel, tempPanel},
		RuneWidth: runeWidth,
	}
	if _, err := tb.Run(); err != nil {
		return // cancelled — nothing applied, nothing printed
	}

	changed := false
	if sel := modelPanel.Selected(); len(sel) > 0 && modelValues[sel[0]] != "" && modelValues[sel[0]] != p.Model() {
		p.SetModel(modelValues[sel[0]])
		sw.SetModel(modelValues[sel[0]])
		DimStyle.Fprintf(w, "Model switched to %s\n", modelValues[sel[0]])
		changed = true
	}
	if sel := contextPanel.Selected(); len(sel) > 0 && windows[sel[0]] != budget.window {
		budget.setWindow(windows[sel[0]])
		sw.SetContextWindow(windows[sel[0]])
		DimStyle.Fprintf(w, "Context window: %s\n", budget.status())
		changed = true
	}
	if tunable {
		if sel := effortPanel.Selected(); len(sel) > 0 && levels[sel[0]] != tun.Effort() {
			tun.SetEffort(levels[sel[0]])
			sw.SetEffort(levels[sel[0]])
			DimStyle.Fprintf(w, "Effort: %s\n", effortLabel(levels[sel[0]]))
			changed = true
		}
		if v := tempPanel.Value(); !floatPtrEqual(v, tun.Temperature()) {
			tun.SetTemperature(v)
			sw.SetTemperature(v)
			DimStyle.Fprintf(w, "Temperature: %s\n", formatTemperature(v))
			changed = true
		}
	}
	if !changed {
		DimStyle.Fprintln(w, "No changes.")
	}
}
