package chat

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"chatchain/provider"

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

// formatTokens renders a token count compactly: 128000→"128k", 1500000→"1.5m".
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "m"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "k"
	default:
		return strconv.Itoa(n)
	}
}

func trimZero(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
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
		for range m.Attachments {
			total += 256 // images/PDFs aren't text-tokenizable; nominal cost
		}
	}
	return total
}

// contextBudget tracks the model context window and current usage. used prefers
// the provider's real reported usage and falls back to the local tokenizer.
type contextBudget struct {
	window    int
	used      int
	haveUsage bool
	counter   *tokenCounter
}

func newContextBudget(window int) *contextBudget {
	if window <= 0 {
		window = defaultContextWindow
	}
	return &contextBudget{window: window, counter: newTokenCounter()}
}

// update refreshes current usage after a turn: real usage if the provider
// reports it, else a local tokenizer count over the full history.
func (b *contextBudget) update(p provider.Provider, history []provider.Message) {
	if ur, ok := p.(provider.UsageReporter); ok {
		if in, out, ok := ur.LastUsage(); ok {
			b.used = in + out
			b.haveUsage = true
			return
		}
	}
	b.used = b.counter.countMessages(history)
	b.haveUsage = false
}

func (b *contextBudget) setWindow(n int) { b.window = n }

// budgetSnap captures usage at a turn boundary so a failed or retried turn
// can roll its live estimates back along with its messages.
type budgetSnap struct {
	used      int
	haveUsage bool
}

func (b *contextBudget) snap() budgetSnap     { return budgetSnap{b.used, b.haveUsage} }
func (b *contextBudget) restore(s budgetSnap) { b.used, b.haveUsage = s.used, s.haveUsage }

// liveAdd folds an in-flight estimate into used while a turn streams. The
// figure is provisional — haveUsage drops so the status line shows ≈ — until
// a settle point (round end, turn end) replaces it with real usage.
func (b *contextBudget) liveAdd(n int) {
	if n <= 0 {
		return
	}
	b.used += n
	b.haveUsage = false
}

// reseed recomputes usage from history with the local tokenizer and drops any
// provider-reported figure. Used when the active conversation is replaced
// wholesale — compaction, or resuming a different session — so the provider's
// last reported usage no longer describes what will be sent.
func (b *contextBudget) reseed(history []provider.Message) {
	b.used = b.counter.countMessages(history)
	b.haveUsage = false
}

// shouldCompact reports whether the next request (current usage + `extra` tokens
// of new, not-yet-sent content) would reach the compaction threshold.
func (b *contextBudget) shouldCompact(extra int) bool {
	if b.window <= 0 {
		return false
	}
	return b.used+extra >= b.window*compactThresholdPercent/100
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
	return declinedAt == 0 || b.used+extra >= declinedAt+b.window*compactSnoozePercent/100
}

// ctxMeterPushEvery throttles mid-stream status pushes: the render sink
// already sends one UI message per delta, so the meter must not double that.
const ctxMeterPushEvery = 250 * time.Millisecond

// ctxMeter moves the status line's context figure DURING a turn instead of
// once at its end: streamed output ticks the estimate up, appended messages
// (the user's send, tool results) land immediately, and each completed round
// settles the figure with the provider's real usage. A nil meter (provider
// without token accounting) is a no-op everywhere.
//
// Every method runs on the chat-loop goroutine — the stream tees sit on the
// READ side of the render pipes, not on the provider's writer — so the
// budget needs no lock.
type ctxMeter struct {
	budget  *contextBudget
	p       provider.Provider // bound at construction: the settle source
	push    func()
	every   time.Duration
	last    time.Time
	pending strings.Builder // deltas batched since the last flush: counting
	// per SSE chunk would overcount (chunk boundaries split tokens)
}

func newCtxMeter(budget *contextBudget, p provider.Provider, push func()) *ctxMeter {
	return &ctxMeter{budget: budget, p: p, push: push, every: ctxMeterPushEvery}
}

// Write is the stream tee: batch the delta, and on the throttle boundary
// fold the batch into the estimate and push the status line.
func (m *ctxMeter) Write(b []byte) (int, error) {
	if m == nil {
		return len(b), nil
	}
	m.pending.Write(b)
	if time.Since(m.last) >= m.every {
		m.last = time.Now()
		m.flushPending()
		m.push()
	}
	return len(b), nil
}

func (m *ctxMeter) flushPending() {
	if m.pending.Len() == 0 {
		return
	}
	m.budget.liveAdd(m.budget.counter.count(m.pending.String()))
	m.pending.Reset()
}

// note folds freshly appended messages — the user's send, a tool result —
// into the estimate right away: a 50k-token file read should move the meter
// when it lands, not a round later.
func (m *ctxMeter) note(msgs ...provider.Message) {
	if m == nil {
		return
	}
	m.flushPending()
	m.budget.liveAdd(m.budget.counter.countMessages(msgs))
	m.push()
}

// settle replaces the running estimate with the provider's real usage at a
// round boundary (stream just ended, LastUsage is fresh).
func (m *ctxMeter) settle(history []provider.Message) {
	if m == nil {
		return
	}
	m.pending.Reset() // superseded by the real figure
	m.budget.update(m.p, history)
	m.push()
}

// reset drops any unflushed batch at a turn boundary so it cannot leak into
// the next turn's estimate. The budget itself is the caller's business.
func (m *ctxMeter) reset() {
	if m == nil {
		return
	}
	m.pending.Reset()
}

// status renders "used / window (pct)"; a leading ≈ marks a local estimate.
func (b *contextBudget) status() string {
	pct := 0
	if b.window > 0 {
		pct = b.used * 100 / b.window
	}
	prefix := ""
	if !b.haveUsage {
		prefix = "≈"
	}
	return fmt.Sprintf("%s%s / %s (%d%%)", prefix, formatTokens(b.used), formatTokens(b.window), pct)
}
