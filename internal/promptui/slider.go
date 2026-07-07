package promptui

import (
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// SliderPanel — a numeric slider with a distinguished "default" state.
// ---------------------------------------------------------------------------

// SliderPanel is a Panel that picks a number from [Min, Max] in Step
// increments, with a distinguished "default" state (nil) meaning the caller
// should omit the parameter entirely — e.g. /model's Temperature tab, where
// default keeps the request byte-identical to today's. Stepping below Min
// falls back to default; stepping up from default enters the range at Min.
type SliderPanel struct {
	// TitleText is the tab label.
	TitleText string
	// Min, Max, Step define the numeric range and the ←/→ increment.
	Min, Max, Step float64
	// RuneWidth returns a rune's display width (CJK = 2); nil means 1 per rune.
	RuneWidth func(rune) int

	value *float64 // nil = default (parameter unset)
}

// NewSliderPanel builds a SliderPanel over [min, max] stepping by step. It
// starts at default (nil) until SetValue seeds it.
func NewSliderPanel(title string, min, max, step float64) *SliderPanel {
	return &SliderPanel{TitleText: title, Min: min, Max: max, Step: step}
}

// Title implements Panel.
func (p *SliderPanel) Title() string { return p.TitleText }

// HelpHint tailors the container's help line to the slider's keymap.
func (p *SliderPanel) HelpHint() string {
	return "←→ adjust · g default · G max · Enter confirm · q/Esc cancel"
}

// SetValue seeds the slider's current value: nil selects default, otherwise a
// copy of *v (the pointed-to value is not aliased).
func (p *SliderPanel) SetValue(v *float64) {
	if v == nil {
		p.value = nil
		return
	}
	c := *v
	p.value = &c
}

// Value returns the slider's result: nil for default, otherwise a copy of the
// chosen number.
func (p *SliderPanel) Value() *float64 {
	if p.value == nil {
		return nil
	}
	v := *p.value
	return &v
}

// HandleKey implements Panel: ←→ (vim hl) step the value, g jumps to default,
// G to Max. Enter is left for the container to commit, so it is never consumed
// here.
func (p *SliderPanel) HandleKey(key rune) (consumed bool) {
	switch {
	case key == KeyEnter:
		return false // container commits
	case key == KeyBackward || key == 'h':
		p.step(-1)
	case key == KeyForward || key == 'l':
		p.step(1)
	case key == 'g':
		p.value = nil // back to default
	case key == 'G':
		v := p.Max
		p.value = &v
	default:
		return false
	}
	return true
}

// step moves the value by dir steps (dir = ±1). From default, +1 enters the
// range at Min (and -1 stays default); stepping below Min falls back to
// default; stepping above Max clamps to Max. The new value is derived from an
// integer step index rather than accumulated, so repeated 0.1 steps never
// build up binary noise.
func (p *SliderPanel) step(dir int) {
	if p.value == nil {
		if dir > 0 {
			v := p.Min
			p.value = &v
		}
		return
	}
	if p.Step <= 0 {
		return
	}
	i := int(math.Round((*p.value-p.Min)/p.Step)) + dir
	if i < 0 {
		p.value = nil // below Min → default
		return
	}
	v := p.snap(p.Min + float64(i)*p.Step)
	if v > p.Max {
		v = p.Max
	}
	p.value = &v
}

// snap rounds v onto Step's decimal grid so a single index*Step multiplication
// never surfaces float noise (0.1*3 → 0.30000000000000004) in the shown value.
func (p *SliderPanel) snap(v float64) float64 {
	if p.Step <= 0 {
		return v
	}
	scale := math.Pow(10, float64(stepDecimals(p.Step)))
	return math.Round(v*scale) / scale
}

// stepDecimals counts step's decimal digits ("0.05" → 2) via the shortest
// round-tripping formatting.
func stepDecimals(step float64) int {
	s := formatFloat(step)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// formatFloat renders v with the fewest digits that round-trip ("0.3", "2").
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Render implements Panel: a value line — "default (provider decides)" when
// unset, the number otherwise — above a horizontal bar whose knob reflects the
// value's position between Min and Max, e.g. "0.0 ├────●──────┤ 2.0".
func (p *SliderPanel) Render(width, height int) []string {
	var val string
	if p.value == nil {
		val = FGFaintStyle(truncate("default (provider decides)", width-2, p.rw))
	} else {
		val = Styler(FGCyan)(truncate(formatFloat(*p.value), width-2, p.rw))
	}
	out := []string{"  " + val}
	if height < 2 {
		return out
	}
	if bar := p.bar(width); bar != "" {
		out = append(out, bar)
	}
	return out
}

// bar renders "min ├───●───┤ max" with the knob at (value-min)/(max-min) of
// the track, or a knobless track in the default state. The track is one column
// per step position (compact, never stretched to the full width) so a single
// keypress moves the knob exactly one cell; it only shrinks when the terminal
// is too narrow. The labels are dropped when the width can't fit a usable
// track, and the bar disappears entirely below that.
func (p *SliderPanel) bar(width int) string {
	lo, hi := formatFloat(p.Min), formatFloat(p.Max)
	avail := width - p.strWidth(lo) - p.strWidth(hi) - 6 // indent + "lo ├" + "┤ hi"
	if avail < 3 {
		lo, hi = "", ""
		avail = width - 4
	}
	track := p.positions()
	if track > avail {
		track = avail
	}
	if track < 1 {
		return ""
	}
	knob := -1
	if p.value != nil && p.Max > p.Min {
		frac := (*p.value - p.Min) / (p.Max - p.Min)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		knob = int(math.Round(frac * float64(track-1)))
	}
	var b strings.Builder
	b.WriteString("  ")
	if lo != "" {
		b.WriteString(FGFaintStyle(lo + " "))
	}
	b.WriteString(FGFaintStyle("├"))
	for i := 0; i < track; i++ {
		if i == knob {
			b.WriteString(Styler(FGCyan)("●"))
		} else {
			b.WriteString(FGFaintStyle("─"))
		}
	}
	b.WriteString(FGFaintStyle("┤"))
	if hi != "" {
		b.WriteString(FGFaintStyle(" " + hi))
	}
	return b.String()
}

// positions is the number of discrete values on the track — one column per
// step — so the bar is exactly as long as it has values to show.
func (p *SliderPanel) positions() int {
	if p.Step > 0 && p.Max > p.Min {
		return int(math.Round((p.Max-p.Min)/p.Step)) + 1
	}
	return 21
}

// strWidth is the display width of s via the injected RuneWidth.
func (p *SliderPanel) strWidth(s string) int {
	w := 0
	for _, r := range s {
		w += p.rw(r)
	}
	return w
}

func (p *SliderPanel) rw(r rune) int {
	if p.RuneWidth != nil {
		return p.RuneWidth(r)
	}
	return 1
}
