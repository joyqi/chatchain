package host

import (
	"errors"
	"testing"
)

type stateOnly struct{ states []State }

func (s *stateOnly) Name() string     { return "state-only" }
func (s *stateOnly) SetState(t State) { s.states = append(s.states, t) }

type full struct {
	states []State
	events []Event
}

func (f *full) Name() string     { return "full" }
func (f *full) SetState(t State) { f.states = append(f.states, t) }
func (f *full) Notify(e Event)   { f.events = append(f.events, e) }

// TestPresenterPerCapabilityFallback is the design's core rule: resolution
// happens PER CAPABILITY, not per host. A host owning state does not steal
// notifications — those fall through to the next one that implements them.
func TestPresenterPerCapabilityFallback(t *testing.T) {
	so, fb := &stateOnly{}, &full{}
	p := &Presenter{hosts: []Host{so, fb}, notifyOn: true}

	p.SetState(StateBusy)
	p.Notify(Event{Kind: KindDone, Text: "iota: done"})

	if len(so.states) != 1 || so.states[0] != StateBusy {
		t.Errorf("specialized host states = %v, want [Busy]", so.states)
	}
	if len(fb.states) != 0 {
		t.Errorf("fallback got states %v although a host owns the capability", fb.states)
	}
	if len(fb.events) != 1 || fb.events[0].Text != "iota: done" {
		t.Errorf("fallback events = %v, want the ping it alone can deliver", fb.events)
	}
}

// TestPresenterDedupsStates: command dispatch re-asserts Idle liberally, and
// a host may pay per update (cmux spawns a process) — repeats are dropped.
func TestPresenterDedupsStates(t *testing.T) {
	fb := &full{}
	p := &Presenter{hosts: []Host{fb}, notifyOn: true}

	p.SetState(StateIdle) // initial state: a no-op
	p.SetState(StateBusy)
	p.SetState(StateBusy)
	p.SetState(StateIdle)

	want := []State{StateBusy, StateIdle}
	if len(fb.states) != len(want) || fb.states[0] != want[0] || fb.states[1] != want[1] {
		t.Errorf("states = %v, want %v", fb.states, want)
	}
}

// TestPresenterNotifySwitch: config `notify: false` silences every host.
func TestPresenterNotifySwitch(t *testing.T) {
	fb := &full{}
	p := &Presenter{hosts: []Host{fb}, notifyOn: false}
	p.Notify(Event{Kind: KindFailed, Text: "x"})
	if len(fb.events) != 0 {
		t.Errorf("notify off but events delivered: %v", fb.events)
	}
}

type closerHost struct {
	name  string
	order *[]string
}

func (c *closerHost) Name() string { return c.name }
func (c *closerHost) Close() error {
	*c.order = append(*c.order, c.name)
	return nil
}

// TestPresenterCloseReverseOrder: teardown unwinds detection order.
func TestPresenterCloseReverseOrder(t *testing.T) {
	var order []string
	p := &Presenter{hosts: []Host{&closerHost{"a", &order}, &closerHost{"b", &order}}}
	p.Close()
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Errorf("close order = %v, want [b a]", order)
	}
}

// TestNewPresenterDetects: the registry contributes detected hosts ahead of
// the fallback; an absent environment leaves the fallback alone.
func TestNewPresenterDetects(t *testing.T) {
	none := Env{
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("absent") },
	}
	p := NewPresenter(none, &full{}, true)
	if len(p.hosts) != 1 || p.hosts[0].Name() != "full" {
		t.Fatalf("bare env: hosts = %d, want the fallback alone", len(p.hosts))
	}

	cmuxEnv := Env{
		Getenv: func(k string) string {
			if k == "CMUX_SURFACE_ID" {
				return "surface-1"
			}
			return ""
		},
		LookPath: func(string) (string, error) { return "/usr/bin/true", nil },
	}
	p = NewPresenter(cmuxEnv, &full{}, true)
	if len(p.hosts) != 2 || p.hosts[0].Name() != "cmux" {
		t.Fatalf("cmux env: hosts = %v, want cmux before the fallback", len(p.hosts))
	}
	p.Close() // flushes the worker through /usr/bin/true
}
