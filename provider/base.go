package provider

// baseProvider holds the state and methods common to the SDK-backed providers:
// the provider type tag, active model, sampling temperature, reasoning effort,
// and the token usage from the most recent API call. Each provider embeds it
// (so model/temperature/lastInput/... are promoted and used unchanged) and sets
// providerType in its constructor. It satisfies the Type/Model/SetModel parts
// of Provider, Tunable, and the LastUsage part of UsageReporter via field
// promotion. A nil temperature or "" effort means the parameter is omitted
// from requests entirely.
type baseProvider struct {
	providerType string
	model        string
	temperature  *float64
	topP         *float64
	effort       string
	lastUsage    Usage
	lastUsageOK  bool
	toolObserver func(name, argsDelta string)
}

func (b *baseProvider) Type() string              { return b.providerType }
func (b *baseProvider) Model() string             { return b.model }
func (b *baseProvider) SetModel(m string)         { b.model = m }
func (b *baseProvider) SetTemperature(t *float64) { b.temperature = t }
func (b *baseProvider) Temperature() *float64     { return b.temperature }
func (b *baseProvider) SetTopP(p *float64)        { b.topP = p }
func (b *baseProvider) SetEffort(level string)    { b.effort = level }
func (b *baseProvider) Effort() string            { return b.effort }

// LastUsage reports the token counts of the most recent API call. Every call
// path (streaming and unary alike) clears lastUsageOK on entry and sets it
// only when the wire actually carried usage — so a call that reported nothing
// reads as "unknown" instead of silently re-reporting the previous call's
// figures, which the session's cumulative accounting would double-count.
func (b *baseProvider) LastUsage() (int, int, bool) {
	return b.lastUsage.Input, b.lastUsage.Output, b.lastUsageOK
}

// LastUsageFull is LastUsage with the cache counts and provider total kept —
// context-window math reads this one (see Usage.ContextTokens).
func (b *baseProvider) LastUsageFull() (Usage, bool) { return b.lastUsage, b.lastUsageOK }

// setUsage records a finished call's accounting. Dialects call it with
// whatever the wire carried; absent fields stay zero.
func (b *baseProvider) setUsage(u Usage) { b.lastUsage, b.lastUsageOK = u, true }

// ResetUsage clears the last-call token figures (e.g. when resuming a different
// session) so LastUsage reports nothing until the next API call.
func (b *baseProvider) ResetUsage() { b.lastUsage, b.lastUsageOK = Usage{}, false }

// SetToolCallObserver installs (or clears, with nil) the tool-call streaming
// observer. The chat layer sets it before a streaming turn and clears it once
// the stream goroutine has finished — no locking needed under that contract.
func (b *baseProvider) SetToolCallObserver(fn func(name, argsDelta string)) { b.toolObserver = fn }

// notifyToolDelta reports one streamed tool-call fragment to the observer.
func (b *baseProvider) notifyToolDelta(name, argsDelta string) {
	if b.toolObserver != nil {
		b.toolObserver(name, argsDelta)
	}
}
