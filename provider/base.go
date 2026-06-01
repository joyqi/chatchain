package provider

// baseProvider holds the state and methods common to the SDK-backed providers:
// the provider type tag, active model, sampling temperature, and the token usage
// from the most recent API call. Each provider embeds it (so model/temperature/
// lastInput/... are promoted and used unchanged) and sets providerType in its
// constructor. It satisfies the Type/Model/SetModel parts of Provider and the
// LastUsage part of UsageReporter via field promotion.
//
// OpenClaw does not embed this — its "model" is an agent ID over WebSocket and
// it reports no usage, so it keeps bespoke accessors.
type baseProvider struct {
	providerType string
	model        string
	temperature  *float64
	lastInput    int
	lastOutput   int
	lastUsageOK  bool
}

func (b *baseProvider) Type() string                { return b.providerType }
func (b *baseProvider) Model() string               { return b.model }
func (b *baseProvider) SetModel(m string)           { b.model = m }
func (b *baseProvider) LastUsage() (int, int, bool) { return b.lastInput, b.lastOutput, b.lastUsageOK }
