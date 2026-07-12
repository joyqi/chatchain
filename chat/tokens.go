package chat

import (
	"fmt"
	"strconv"
	"strings"

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

// status renders "used / window (pct)"; a leading ≈ marks a local estimate.
func (b *contextBudget) status() string { return b.statusWithDraft(0) }

// statusWithDraft renders usage like status() but adds `draft` not-yet-sent
// tokens (the message currently being composed) to the used figure, so the
// input composer can show the context window filling live as the user types.
func (b *contextBudget) statusWithDraft(draft int) string {
	used := b.used + draft
	pct := 0
	if b.window > 0 {
		pct = used * 100 / b.window
	}
	prefix := ""
	if !b.haveUsage {
		prefix = "≈"
	}
	return fmt.Sprintf("%s%s / %s (%d%%)", prefix, formatTokens(used), formatTokens(b.window), pct)
}
