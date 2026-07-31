// Package host adapts the chat session's terminal-visible signals — its
// lifecycle state and attention pings — to whatever terminal environment
// ("host") the process runs inside: the plain ANSI terminal, cmux, and
// whatever comes next (tmux, iTerm2 …).
//
// Detection runs once at startup, most specific host first. A detected host
// advertises what it can display through the optional capability interfaces
// (StateReporter, Notifier, io.Closer), and the Presenter resolves every
// signal PER CAPABILITY: the first detected host implementing it wins, and
// the ANSI fallback — always present, always last — covers the rest. Adding
// a host is one file with a Detector in the registry; taking a channel over
// is implementing its interface.
package host

import (
	"io"
	"os"
	"os/exec"
)

// State is the session lifecycle as a host displays it.
type State int

const (
	StateIdle       State = iota
	StateBusy             // turn streaming / tools running
	StateNeedsInput       // blocked on the user (approval gate)
	StateError            // turn failed; stands until the user acts
)

// Kind classifies an attention ping.
type Kind int

const (
	KindNeedsInput Kind = iota
	KindDone
	KindFailed
)

// Event is one attention ping. Text is always a content digest (the answer's
// first line, the error headline, the tool asking) — never a fixed phrase:
// the user deciding whether to switch back deserves a peek.
type Event struct {
	Kind Kind
	Text string
}

// Host is a detected terminal environment. Capabilities are advertised via
// the optional interfaces below — a Host implementing none is inert.
type Host interface {
	Name() string
}

// StateReporter displays the session state (progress bar, sidebar pill …).
type StateReporter interface {
	SetState(State)
}

// Notifier delivers attention pings. CONTRACT: implementing this takes over
// the WHOLE attention decision, presence included — the Presenter delivers
// events ungated (beyond the global notify switch), and each host applies
// its own policy for "is anyone watching": the ANSI host gates by terminal
// focus reporting, a cmux integration would defer to cmux's notification
// policy, a config-hooks host would not gate at all.
type Notifier interface {
	Notify(Event)
}

// Env is the detection seam: probes read the environment through it, so
// tests drive the whole matrix with fixtures.
type Env struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

// SystemEnv is the real process environment.
func SystemEnv() Env { return Env{Getenv: os.Getenv, LookPath: exec.LookPath} }

// A Detector probes for one host; nil when it is absent.
type Detector func(Env) Host

// detectors is the registry, most specific first. The ANSI fallback is not
// listed — it always exists and the caller supplies it (it wraps the UI).
var detectors = []Detector{detectCmux}

// Presenter fans the chat loop's signals out to the detected hosts. It is
// driven from the chat-loop goroutine only — hosts that need concurrency
// (exec workers) own it internally.
type Presenter struct {
	hosts    []Host
	notifyOn bool
	state    State // last delivered state; duplicate sets are dropped
}

// NewPresenter detects hosts under env and appends fallback last. notify is
// the global attention switch (config `notify`, default on).
func NewPresenter(env Env, fallback Host, notify bool) *Presenter {
	p := &Presenter{notifyOn: notify}
	for _, d := range detectors {
		if h := d(env); h != nil {
			p.hosts = append(p.hosts, h)
		}
	}
	if fallback != nil {
		p.hosts = append(p.hosts, fallback)
	}
	return p
}

// SetState delivers a state change to the first host that displays state.
// Repeats of the current state are dropped — command dispatch re-asserts
// Idle liberally, and a host may pay per update (cmux spawns a process).
func (p *Presenter) SetState(s State) {
	if s == p.state {
		return
	}
	p.state = s
	for _, h := range p.hosts {
		if r, ok := h.(StateReporter); ok {
			r.SetState(s)
			return
		}
	}
}

// Notify delivers an attention ping to the first host that notifies —
// unless the global switch is off.
func (p *Presenter) Notify(e Event) {
	if !p.notifyOn {
		return
	}
	for _, h := range p.hosts {
		if n, ok := h.(Notifier); ok {
			n.Notify(e)
			return
		}
	}
}

// Close tears down hosts that need it (a cmux status row would outlive the
// process unless cleared), in reverse detection order.
func (p *Presenter) Close() {
	for i := len(p.hosts) - 1; i >= 0; i-- {
		if c, ok := p.hosts[i].(io.Closer); ok {
			c.Close()
		}
	}
}
