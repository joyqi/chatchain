package chat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/joyqi/iota/internal/tokfmt"
	"github.com/joyqi/iota/provider"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// defaultContextWindow is the assumed model context size when none is configured.
const defaultContextWindow = 128_000

// ParseWindowSize parses a token count with an optional decimal unit suffix
// (k=1e3, m=1e6, b=1e9, case-insensitive): "200k", "1m", "128000" → tokens.
func ParseWindowSize(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1e3, s[:len(s)-1]
	case 'm':
		mult, s = 1e6, s[:len(s)-1]
	case 'b':
		mult, s = 1e9, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid context window size: %q", s)
	}
	n := int(v * mult)
	if n <= 0 {
		return 0, fmt.Errorf("invalid context window size")
	}
	return n, nil
}

// tokenCounter is a local fallback tokenizer (tiktoken o200k_base, embedded
// offline) for providers that don't report usage and for sizing not-yet-sent
// content. Approximate for non-OpenAI models but fine for window accounting.
type tokenCounter struct{ enc *tiktoken.Tiktoken }

func newTokenCounter() *tokenCounter {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
	if err != nil {
		return &tokenCounter{} // enc nil → chars/4 fallback
	}
	return &tokenCounter{enc: enc}
}

func (c *tokenCounter) count(text string) int {
	if c == nil || c.enc == nil {
		return len(text) / 4
	}
	return len(c.enc.Encode(text, nil, nil))
}

func (c *tokenCounter) countMessages(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += c.count(m.Content) + c.count(m.Reasoning) + 4 // ~per-message framing overhead
		for _, tc := range m.ToolCalls {
			total += c.count(tc.Name)
			for k, v := range tc.Arguments {
				total += c.count(k) + c.count(fmt.Sprintf("%v", v))
			}
		}
		total += len(m.Attachments) * attachmentTokens
	}
	return total
}

// attachmentTokens is what one attachment is assumed to cost: images and PDFs
// are not text-tokenizable, so the estimator needs a stand-in figure. 1200 is
// the ballpark a full-size image actually bills at across providers (and what
// pi assumes, as 4800 chars over its chars/4 heuristic). The 256 used before
// was low by roughly 5x, which let an image-heavy history slip past the
// compaction threshold unnoticed.
const attachmentTokens = 1200

// contextBudget tracks the model context window and current usage as TWO
// quantities that must not be confused:
//
//   - settled: what the last API call actually carried (Usage.ContextTokens),
//     or a local count when the provider reports none. Only a settle point
//     writes it.
//   - pending: an estimate of what has landed SINCE — the user's message, tool
//     results. It is RECOMPUTED from those messages, never accumulated into,
//     so an over-estimate cannot outlive the messages that caused it.
//
// The split is what pi does (real usage + an estimate of the trailing
// messages, recomputed on every render). Folding both into one running total
// meant a stale estimate could only be corrected by the next settle, and
// nothing distinguished "measured" from "guessed" inside the figure.
type contextBudget struct {
	window    int
	settled   int
	pending   int
	haveUsage bool
	counter   *tokenCounter
}

// used is what the next request is projected to carry.
func (b *contextBudget) used() int { return b.settled + b.pending }

func newContextBudget(window int) *contextBudget {
	if window <= 0 {
		window = defaultContextWindow
	}
	return &contextBudget{window: window, counter: newTokenCounter()}
}

// update refreshes current usage after a turn: real usage if the provider
// reports it, else a local tokenizer count over the full history.
//
// The figure is the LAST call's full context occupancy (Usage.ContextTokens,
// which knows each dialect's cache/total contract) — that call carried the
// whole conversation, so it is also what the next request starts from.
func (b *contextBudget) update(p provider.Provider, history []provider.Message) {
	b.pending = 0 // superseded: whatever it estimated is now measured
	if ur, ok := p.(provider.UsageReporter); ok {
		if u, ok := ur.LastUsageFull(); ok {
			b.settled = u.ContextTokens()
			b.haveUsage = true
			return
		}
	}
	b.settled = b.counter.countMessages(history)
	b.haveUsage = false
}

func (b *contextBudget) setWindow(n int) { b.window = n }

// budgetSnap captures usage at a turn boundary so a failed or retried turn
// can roll its live estimates back along with its messages.
type budgetSnap struct {
	settled   int
	pending   int
	haveUsage bool
}

func (b *contextBudget) snap() budgetSnap { return budgetSnap{b.settled, b.pending, b.haveUsage} }
func (b *contextBudget) restore(s budgetSnap) {
	b.settled, b.pending, b.haveUsage = s.settled, s.pending, s.haveUsage
}

// setPending replaces the estimate of what has landed since the last settle.
// A replacement, not an addition: the caller owns the whole set of pending
// messages and hands over their total, so a recount corrects itself.
func (b *contextBudget) setPending(n int) {
	if n < 0 {
		n = 0
	}
	b.pending = n
}

// reseed recomputes usage from history with the local tokenizer and drops any
// provider-reported figure. Used when the active conversation is replaced
// wholesale — compaction, or resuming a different session — so the provider's
// last reported usage no longer describes what will be sent.
func (b *contextBudget) reseed(history []provider.Message) {
	b.settled = b.counter.countMessages(history)
	b.pending = 0
	b.haveUsage = false
}

// threshold is the usage at which the next request should compact first. Two
// rules apply, whichever leaves MORE window to work with:
//
//   - a share of the window (compactThresholdPercent), which is what keeps
//     small windows safe: 80% of 20k still leaves 4k, where a flat reserve
//     would put the trigger at or below zero and compact forever;
//   - the window minus a fixed reserve, which stops large windows from
//     throwing away a fifth of their capacity — 80% of 1M would interrupt the
//     user with 200k still free.
//
// pi uses the reserve rule alone (16384 tokens); the percentage floor is what
// makes it safe across the window sizes iota also has to serve.
func (b *contextBudget) threshold() int {
	pct := b.window * compactThresholdPercent / 100
	if reserve := b.window - compactReserveTokens; reserve > pct {
		return reserve
	}
	return pct
}

// shouldCompact reports whether the next request (current usage + `extra` tokens
// of new, not-yet-sent content) would reach the compaction threshold.
func (b *contextBudget) shouldCompact(extra int) bool {
	if b.window <= 0 {
		return false
	}
	return b.used()+extra >= b.threshold()
}

// compactSnoozePercent is how much of the window usage must grow, after the
// user declines the auto-compaction prompt, before it is offered again.
const compactSnoozePercent = 5

// shouldOfferCompact reports whether the auto-compaction confirmation should be
// offered before the next request: the threshold is reached and, if the user
// declined before (declinedAt = usage recorded at that decline; 0 = never),
// usage has since grown by compactSnoozePercent of the window.
func (b *contextBudget) shouldOfferCompact(extra, declinedAt int) bool {
	if !b.shouldCompact(extra) {
		return false
	}
	return declinedAt == 0 || b.used()+extra >= declinedAt+b.window*compactSnoozePercent/100
}

// ctxMeter keeps the status line's context figure honest across a turn: the
// user's message and every tool result move it the moment they land, and each
// completed round settles it with the provider's real usage. A nil meter
// (provider without token accounting) is a no-op everywhere.
//
// Streamed output is deliberately NOT counted. It was, and it was the least
// trustworthy input the figure had: the reasoning text a provider streams is
// a summary of thinking whose real cost the next request carries in a wholly
// different form, so the meter would climb through a long stream and then
// drop when the settle measured what had actually been sent. pi, Codex and
// crush all move their figure only at message boundaries for the same reason.
//
// Every method runs on the chat-loop goroutine, so the budget needs no lock.
type ctxMeter struct {
	budget *contextBudget
	p      provider.Provider // bound at construction: the settle source
	push   func()
	// since is every message appended after the last settle. pending is
	// recomputed from it rather than accumulated, so a re-estimate corrects
	// itself and a reset cannot leave residue behind in the figure.
	since []provider.Message
	// session accumulates what every API call of this session cost — the
	// status line's ↑/↓ figures. Unlike the budget (which measures what the
	// NEXT request carries) it only grows, and it survives a resume: see
	// record.
	session provider.Usage
}

func newCtxMeter(budget *contextBudget, p provider.Provider, push func()) *ctxMeter {
	return &ctxMeter{budget: budget, p: p, push: push}
}

// note records messages appended since the last settle — the user's send, a
// tool result — and re-estimates the pending total: a 50k-token file read
// should move the meter when it lands, not a round later.
func (m *ctxMeter) note(msgs ...provider.Message) {
	if m == nil {
		return
	}
	m.since = append(m.since, msgs...)
	m.budget.setPending(m.budget.counter.countMessages(m.since))
	m.push()
}

// settle replaces the running estimate with the provider's real usage at a
// round boundary (stream just ended, LastUsage is fresh).
func (m *ctxMeter) settle(history []provider.Message) {
	if m == nil {
		return
	}
	m.since = nil // superseded by the real figure
	m.budget.update(m.p, history)
	m.push()
}

// reset drops the pending set at a turn boundary so it cannot leak into the
// next turn's estimate. The budget itself is the caller's business.
func (m *ctxMeter) reset() {
	if m == nil {
		return
	}
	m.since = nil
	m.budget.setPending(0)
}

// record books the API call that just finished: it stamps msg (the assistant
// message that call produced; nil for callers with no message, e.g. a
// compaction pass) with the usage and folds it into the session totals,
// returning what it recorded. ONE call site owns both sides, so the live
// figures and the ones a resumed session recomputes from its log cannot
// drift.
//
// Every provider clears its usage flag when a call starts, so a call that
// reported nothing records nothing — the previous call's figures can never be
// counted twice.
func (m *ctxMeter) record(msg *provider.Message) *provider.Usage {
	if m == nil {
		return nil
	}
	ur, ok := m.p.(provider.UsageReporter)
	if !ok {
		return nil
	}
	u, ok := ur.LastUsageFull()
	if !ok {
		return nil
	}
	if msg != nil {
		msg.Usage = &u
	}
	m.session.Add(u)
	m.push()
	return &u
}

// totals is the session's cumulative usage (zero on a nil meter — a provider
// without token accounting reports none).
func (m *ctxMeter) totals() provider.Usage {
	if m == nil {
		return provider.Usage{}
	}
	return m.session
}

// seedTotals replaces the cumulative figures wholesale: a resumed session
// starts from what its log adds up to, a switched-to session from its own.
func (m *ctxMeter) seedTotals(u provider.Usage) {
	if m == nil {
		return
	}
	m.session = u
	m.push()
}

// status renders "used / window (pct)"; a leading ≈ marks a local estimate.
func (b *contextBudget) status() string {
	pct := 0
	if b.window > 0 {
		pct = b.used() * 100 / b.window
	}
	prefix := ""
	if !b.haveUsage {
		prefix = "≈"
	}
	return fmt.Sprintf("%s%s / %s (%d%%)", prefix, tokfmt.Tokens(b.used()), tokfmt.Tokens(b.window), pct)
}
