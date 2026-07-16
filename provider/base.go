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
	effort       string
	lastInput    int
	lastOutput   int
	lastUsageOK  bool
	toolObserver func(name, argsDelta string)
}

func (b *baseProvider) Type() string                { return b.providerType }
func (b *baseProvider) Model() string               { return b.model }
func (b *baseProvider) SetModel(m string)           { b.model = m }
func (b *baseProvider) SetTemperature(t *float64)   { b.temperature = t }
func (b *baseProvider) Temperature() *float64       { return b.temperature }
func (b *baseProvider) SetEffort(level string)      { b.effort = level }
func (b *baseProvider) Effort() string              { return b.effort }
func (b *baseProvider) LastUsage() (int, int, bool) { return b.lastInput, b.lastOutput, b.lastUsageOK }

// ResetUsage clears the last-call token figures (e.g. when resuming a different
// session) so LastUsage reports nothing until the next API call.
func (b *baseProvider) ResetUsage() { b.lastInput, b.lastOutput, b.lastUsageOK = 0, 0, false }

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
