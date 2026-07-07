package promptui

import (
	"strings"
	"testing"
)

func TestSliderPanelStepAndFloatSafety(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	if p.Value() != nil {
		t.Fatalf("initial Value = %v, want nil (default)", *p.Value())
	}
	// From default, → enters the range at Min.
	feed(p, KeyForward)
	v := p.Value()
	if v == nil || *v != 0 {
		t.Fatalf("Value after first step = %v, want 0 (Min)", v)
	}
	// Three more 0.1 steps land exactly on 0.3 — no accumulated binary noise.
	feed(p, 'l', 'l', 'l')
	if v = p.Value(); v == nil {
		t.Fatal("Value after stepping up = nil, want 0.3")
	}
	if got := formatFloat(*v); got != "0.3" {
		t.Fatalf("Value after 0.1*3 formats as %q, want \"0.3\"", got)
	}
	// One step back down.
	feed(p, KeyBackward)
	if v = p.Value(); v == nil || formatFloat(*v) != "0.2" {
		t.Fatalf("Value after step down = %v, want 0.2", v)
	}
}

func TestSliderPanelDefaultTransitions(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	// Min → below Min falls back to default (nil).
	feed(p, 'l', 'h')
	if p.Value() != nil {
		t.Fatalf("Value after stepping below Min = %v, want nil (default)", *p.Value())
	}
	// Decreasing from default stays default (the key is still consumed).
	if !p.HandleKey('h') {
		t.Fatal("h from default should be consumed")
	}
	if p.Value() != nil {
		t.Fatal("Value should stay default when decreasing from default")
	}
	// Stepping up from default re-enters at Min.
	feed(p, KeyForward)
	if v := p.Value(); v == nil || *v != 0 {
		t.Fatalf("Value after re-entering = %v, want 0 (Min)", v)
	}
}

func TestSliderPanelClampAtMax(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 1, 0.4)
	seed := 0.8
	p.SetValue(&seed)
	// 0.8 + 0.4 overshoots → clamp to Max; further steps stay there.
	feed(p, 'l', 'l')
	if v := p.Value(); v == nil || *v != 1 {
		t.Fatalf("Value after stepping past Max = %v, want 1 (Max)", v)
	}
	// Stepping down from a clamped Max returns to the grid below it.
	feed(p, 'h')
	if v := p.Value(); v == nil || formatFloat(*v) != "0.8" {
		t.Fatalf("Value after stepping down from Max = %v, want 0.8", v)
	}
}

func TestSliderPanelJumpKeys(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	// G jumps to Max, g back to default.
	feed(p, 'G')
	if v := p.Value(); v == nil || *v != 2 {
		t.Fatalf("Value after G = %v, want 2 (Max)", v)
	}
	feed(p, 'g')
	if p.Value() != nil {
		t.Fatalf("Value after g = %v, want nil (default)", *p.Value())
	}
}

func TestSliderPanelSetValue(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	seed := 0.7
	p.SetValue(&seed)
	v := p.Value()
	if v == nil || *v != 0.7 {
		t.Fatalf("Value after SetValue = %v, want 0.7", v)
	}
	// Value returns a copy: mutating it must not affect the panel.
	*v = 1.5
	if got := p.Value(); got == nil || *got != 0.7 {
		t.Fatalf("Value after mutating a returned copy = %v, want 0.7", got)
	}
	// SetValue(nil) resets to default.
	p.SetValue(nil)
	if p.Value() != nil {
		t.Fatal("Value after SetValue(nil) should be nil (default)")
	}
}

func TestSliderPanelKeyConsumption(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	// Enter is never consumed by the panel (the container commits).
	if p.HandleKey(KeyEnter) {
		t.Fatal("SliderPanel consumed Enter; want container to commit")
	}
	// Keys outside the slider's map are not consumed.
	if p.HandleKey('j') {
		t.Fatal("SliderPanel consumed an unhandled key")
	}
}

// knobOffset counts the track cells left of the knob in a rendered bar, or -1
// when no knob is present (default state).
func knobOffset(bar string) int {
	n := 0
	for _, r := range bar {
		switch r {
		case '●':
			return n
		case '─':
			n++
		}
	}
	return -1
}

func TestSliderPanelRenderKnob(t *testing.T) {
	p := NewSliderPanel("Temperature", 0, 2, 0.1)
	// Default: a dim label and a knobless bar.
	lines := p.Render(40, 5)
	if len(lines) != 2 {
		t.Fatalf("Render lines = %d, want 2 (value + bar)", len(lines))
	}
	if !strings.Contains(lines[0], "default (provider decides)") {
		t.Fatalf("default value line = %q, want the default label", lines[0])
	}
	if knobOffset(lines[1]) != -1 {
		t.Fatalf("default bar = %q, want no knob", lines[1])
	}
	// Min puts the knob at the left edge; Max moves it strictly right of that.
	v := 0.0
	p.SetValue(&v)
	left := knobOffset(p.Render(40, 5)[1])
	feed(p, 'G')
	right := knobOffset(p.Render(40, 5)[1])
	if left != 0 {
		t.Fatalf("knob offset at Min = %d, want 0", left)
	}
	if right <= left {
		t.Fatalf("knob offset at Max = %d, want > %d (knob must move right)", right, left)
	}
}

func TestListPanelSetCursor(t *testing.T) {
	p := NewListPanel("x", []string{"a", "b", "c"}, false)
	p.SetCursor(2)
	if p.Cursor() != 2 {
		t.Fatalf("cursor after SetCursor(2) = %d, want 2", p.Cursor())
	}
	p.SetCursor(99) // past the end → clamp to the last row
	if p.Cursor() != 2 {
		t.Fatalf("cursor after SetCursor(99) = %d, want 2 (clamped)", p.Cursor())
	}
	p.SetCursor(-3) // negative → clamp to 0
	if p.Cursor() != 0 {
		t.Fatalf("cursor after SetCursor(-3) = %d, want 0 (clamped)", p.Cursor())
	}
	// Empty list: SetCursor stays a safe 0.
	e := NewListPanel("x", nil, false)
	e.SetCursor(5)
	if e.Cursor() != 0 {
		t.Fatalf("cursor on empty list = %d, want 0", e.Cursor())
	}
}
