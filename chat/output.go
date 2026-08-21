package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"chatchain/provider"
)

// The machine-readable report for a non-interactive (-m) run.
//
// -m printed the reply and nothing else, which is the right answer for a
// human at a terminal and the wrong one for everything else: what the run
// cost, how many round trips it took and why it stopped were recoverable only
// by reading prose. Anything driving this binary as a subprocess — CI, a
// pipeline, a parent agent treating it as a sub-agent — needs those as data.
//
// The shape follows the two CLIs that already settled this. Usage rides on
// the round that incurred it (Codex's turn.completed.usage) and the run ends
// with one object carrying the reply and the totals (Claude Code's result
// message). It deliberately stops short of their event STREAMS: nothing here
// renders a running command's output yet, so per-round events would have no
// reader, and a format with no reader rots.

// OutputFormat selects how a non-interactive run reports itself.
type OutputFormat string

const (
	// OutputText prints the reply alone — the historical -m behaviour, and
	// still the default. It is also the right choice for a sub-agent: the
	// point of delegating is that the answer reaches the caller's context
	// without the transcript that produced it.
	OutputText OutputFormat = "text"
	// OutputJSON replaces that with a single result object.
	OutputJSON OutputFormat = "json"
)

// ParseOutputFormat validates the flag value. An unknown format is an error
// rather than a quiet fall back to text: a caller that asked for JSON and
// received prose would parse the prose as data.
func ParseOutputFormat(s string) (OutputFormat, error) {
	switch f := OutputFormat(strings.TrimSpace(s)); f {
	case "", OutputText:
		return OutputText, nil
	case OutputJSON:
		return OutputJSON, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want text or json)", s)
	}
}

// TokenUsage is provider.Usage in wire form, named in the dialect-neutral
// vocabulary the internal accounting already uses so a consumer never has to
// know which provider produced the numbers.
//
// TotalTokens carries the same tell the internal type does: every dialect
// that folds cache reads INTO input also reports a total, and anthropic — the
// one that files them beside input — reports none. Summing across rounds
// preserves that, so a zero total still means "add the parts up yourself"
// rather than "this run was free".
type TokenUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func tokenUsage(u provider.Usage) TokenUsage {
	return TokenUsage{
		InputTokens:      u.Input,
		OutputTokens:     u.Output,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
		TotalTokens:      u.Total,
	}
}

func (t *TokenUsage) add(u provider.Usage) {
	t.InputTokens += u.Input
	t.OutputTokens += u.Output
	t.CacheReadTokens += u.CacheRead
	t.CacheWriteTokens += u.CacheWrite
	t.TotalTokens += u.Total
}

// RoundReport is one API round: what it cost and which tools it dispatched.
// Per-round granularity is what makes a cost surprise diagnosable — a single
// total cannot tell one expensive round from twenty cheap ones.
type RoundReport struct {
	Round int        `json:"round"` // 1-based
	Tools []string   `json:"tools,omitempty"`
	Usage TokenUsage `json:"usage"`
}

// RunReport is the whole run, emitted once. Type is constant, so a consumer
// reading a mixed stream can branch on it the same way it will when the
// streaming format arrives.
type RunReport struct {
	Type        string        `json:"type"` // always "result"
	Provider    string        `json:"provider"`
	Model       string        `json:"model"`
	Reply       string        `json:"reply"`
	Error       string        `json:"error,omitempty"`
	Rounds      int           `json:"rounds"`
	DurationMS  int64         `json:"duration_ms"`
	Usage       TokenUsage    `json:"usage"`
	RoundUsage  []RoundReport `json:"round_usage,omitempty"`
	Images      []string      `json:"images,omitempty"`
	ImageErrors []string      `json:"image_errors,omitempty"`
}

// runRecorder accumulates what the tool loop learns as it runs. The loop
// stays output-agnostic: it records unconditionally (a few ints per round),
// and Once decides whether any of it is printed.
type runRecorder struct {
	started time.Time
	rounds  []RoundReport
	total   TokenUsage
}

func newRunRecorder() *runRecorder { return &runRecorder{started: time.Now()} }

// observe records one completed round. Usage comes from the provider's
// last-call accounting (the UsageReporter capability, read immediately after
// the call so it is still that round's). A provider without the capability
// contributes a zero-usage round rather than no round at all, keeping the
// round COUNT truthful even where the cost is unknowable.
func (r *runRecorder) observe(p any, tools []string) {
	rr := RoundReport{Round: len(r.rounds) + 1, Tools: tools}
	if ur, ok := p.(provider.UsageReporter); ok {
		if u, ok := ur.LastUsageFull(); ok {
			rr.Usage = tokenUsage(u)
			r.total.add(u)
		}
	}
	r.rounds = append(r.rounds, rr)
}

// report closes the run. A failed run still reports: the rounds that did
// complete were billed, and hiding them would make exactly the runs worth
// investigating the ones with no numbers.
func (r *runRecorder) report(p provider.Provider, reply string, images, imageErrs []string, err error) RunReport {
	rep := RunReport{
		Type:        "result",
		Provider:    p.Type(),
		Model:       p.Model(),
		Reply:       reply,
		Rounds:      len(r.rounds),
		DurationMS:  time.Since(r.started).Milliseconds(),
		Usage:       r.total,
		RoundUsage:  r.rounds,
		Images:      images,
		ImageErrors: imageErrs,
	}
	if err != nil {
		rep.Error = err.Error()
	}
	return rep
}

// writeReport emits the report as indented JSON. Indented because this format
// is defined by being ONE object — readability costs nothing here, and the
// streaming format that will need line-per-event compactness does not exist
// yet. HTML escaping is off: replies carry code, and < helps no one.
func writeReport(w io.Writer, rep RunReport) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// toolNames lists a round's dispatched tools for its report, in call order
// and with repeats kept — four read_file calls are a different round from one.
func toolNames(calls []provider.ToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	names := make([]string, 0, len(calls))
	for _, tc := range calls {
		names = append(names, tc.Name)
	}
	return names
}
