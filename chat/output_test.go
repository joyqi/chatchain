package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"chatchain/provider"
	"chatchain/tool"
)

func TestParseOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    OutputFormat
		wantErr bool
	}{
		{"", OutputText, false},
		{"text", OutputText, false},
		{"json", OutputJSON, false},
		{" json ", OutputJSON, false},
		// An unknown format must not degrade to text: a caller that asked for
		// JSON and received prose would parse the prose as data.
		{"stream-json", "", true},
		{"yaml", "", true},
	} {
		got, err := ParseOutputFormat(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseOutputFormat(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("ParseOutputFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The dialect tell rides through aggregation: anthropic reports cache counts
// BESIDE input and no total, so a summed total of zero still has to mean "add
// the parts up yourself" rather than "this run was free".
func TestTokenUsageSumPreservesTheAbsentTotal(t *testing.T) {
	var anthropic TokenUsage
	anthropic.add(provider.Usage{Input: 100, Output: 20, CacheRead: 900})
	anthropic.add(provider.Usage{Input: 150, Output: 30, CacheRead: 1000})
	if anthropic.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0 (the no-total dialect tell)", anthropic.TotalTokens)
	}
	if anthropic.CacheReadTokens != 1900 || anthropic.InputTokens != 250 {
		t.Errorf("parts = %+v, want input 250 / cache_read 1900", anthropic)
	}

	var openai TokenUsage
	openai.add(provider.Usage{Input: 1000, Output: 50, CacheRead: 800, Total: 1050})
	openai.add(provider.Usage{Input: 1200, Output: 60, CacheRead: 900, Total: 1260})
	if openai.TotalTokens != 2310 {
		t.Errorf("TotalTokens = %d, want 2310", openai.TotalTokens)
	}
}

// reportingProvider answers with tool calls for stopAfter rounds, then a final
// message, reporting per-call usage that differs each round so the report can
// be checked for having attributed each one correctly.
type reportingProvider struct {
	calls     int
	stopAfter int
	failOn    int // 1-based round to fail on; 0 = never
	last      provider.Usage
}

func (p *reportingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *reportingProvider) Chat(context.Context, []provider.Message) (string, error) {
	p.calls++
	p.last = provider.Usage{Input: 10, Output: 2, Total: 12}
	return "hi", nil
}
func (p *reportingProvider) StreamChat(context.Context, []provider.Message, io.Writer, io.WriteCloser) (string, string, error) {
	return "", "", nil
}
func (p *reportingProvider) Type() string     { return "openai" }
func (p *reportingProvider) Model() string    { return "gpt-test" }
func (p *reportingProvider) SetModel(string)  {}
func (p *reportingProvider) LastUsage() (int, int, bool) {
	return p.last.Input, p.last.Output, true
}
func (p *reportingProvider) LastUsageFull() (provider.Usage, bool) { return p.last, true }

func (p *reportingProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.calls++
	if p.failOn == p.calls {
		return "", "", nil, errors.New("upstream exploded")
	}
	// Distinct per round, so a report that mixed rounds up would show it.
	p.last = provider.Usage{Input: 100 * p.calls, Output: 10 * p.calls, CacheRead: p.calls, Total: 110 * p.calls}
	if p.calls > p.stopAfter {
		return "final answer", "", nil, nil
	}
	return "", "", []provider.ToolCall{{ID: fmt.Sprintf("c%d", p.calls), Name: "noop", Arguments: map[string]any{}}}, nil
}

func runJSON(t *testing.T, p provider.Provider, dispatch tool.Dispatcher) (RunReport, error) {
	t.Helper()
	var buf bytes.Buffer
	err := Once(context.Background(), p, "go", "", dispatch, AgentOptions{}, 0, OutputJSON, &buf)
	var rep RunReport
	if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", jerr, buf.String())
	}
	return rep, err
}

func TestOnceJSONReportsEveryRound(t *testing.T) {
	p := &reportingProvider{stopAfter: 2}
	rep, err := runJSON(t, p, noopDispatcher{})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if rep.Type != "result" || rep.Provider != "openai" || rep.Model != "gpt-test" {
		t.Errorf("envelope = %+v, want type/provider/model filled", rep)
	}
	if rep.Reply != "final answer" {
		t.Errorf("Reply = %q", rep.Reply)
	}
	// Two tool rounds plus the answering round.
	if rep.Rounds != 3 || len(rep.RoundUsage) != 3 {
		t.Fatalf("Rounds = %d / %d entries, want 3", rep.Rounds, len(rep.RoundUsage))
	}
	// Usage is attributed to the round that incurred it, not smeared.
	for i, r := range rep.RoundUsage {
		if r.Round != i+1 {
			t.Errorf("RoundUsage[%d].Round = %d, want %d", i, r.Round, i+1)
		}
		if want := 100 * (i + 1); r.Usage.InputTokens != want {
			t.Errorf("round %d input = %d, want %d", i+1, r.Usage.InputTokens, want)
		}
	}
	// The two tool rounds name their tool; the answering round names none.
	if got := rep.RoundUsage[0].Tools; len(got) != 1 || got[0] != "noop" {
		t.Errorf("round 1 tools = %v, want [noop]", got)
	}
	if got := rep.RoundUsage[2].Tools; len(got) != 0 {
		t.Errorf("final round tools = %v, want none", got)
	}
	// Totals are the sum of the parts.
	if rep.Usage.InputTokens != 600 || rep.Usage.OutputTokens != 60 || rep.Usage.TotalTokens != 660 {
		t.Errorf("totals = %+v, want input 600 / output 60 / total 660", rep.Usage)
	}
}

// A failed run still reports: the rounds before the failure were billed, and
// suppressing them would leave exactly the runs worth investigating with no
// numbers. The error travels out too, so the exit status keeps its meaning.
func TestOnceJSONReportsAFailedRun(t *testing.T) {
	p := &reportingProvider{stopAfter: 5, failOn: 3}
	rep, err := runJSON(t, p, noopDispatcher{})
	if err == nil {
		t.Fatal("Once must still return the error in JSON mode")
	}
	if !strings.Contains(rep.Error, "upstream exploded") {
		t.Errorf("Error = %q, want the upstream failure", rep.Error)
	}
	if rep.Reply != "" {
		t.Errorf("Reply = %q, want empty on failure", rep.Reply)
	}
	// Rounds 1 and 2 completed and were paid for; round 3 never returned.
	if rep.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2 completed rounds", rep.Rounds)
	}
	if rep.Usage.InputTokens != 300 {
		t.Errorf("input = %d, want 300 (rounds 1-2 only)", rep.Usage.InputTokens)
	}
}

// Text mode is unchanged: the reply, alone, with no report anywhere near it.
func TestOnceTextModeStaysBare(t *testing.T) {
	p := &reportingProvider{stopAfter: 1}
	var buf bytes.Buffer
	if err := Once(context.Background(), p, "go", "", noopDispatcher{}, AgentOptions{}, 0, OutputText, &buf); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if got := buf.String(); got != "final answer\n" {
		t.Errorf("text output = %q, want the reply alone", got)
	}
}

// The wire names are the contract a consumer parses against; a rename would
// silently break every caller, so they are pinned here.
func TestRunReportWireNames(t *testing.T) {
	var buf bytes.Buffer
	rep := RunReport{Type: "result", Rounds: 1}
	rep.Usage.CacheWriteTokens = 7
	if err := writeReport(&buf, rep); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"type"`, `"provider"`, `"model"`, `"reply"`, `"rounds"`, `"duration_ms"`,
		`"input_tokens"`, `"output_tokens"`, `"cache_read_tokens"`, `"cache_write_tokens"`, `"total_tokens"`,
	} {
		if !strings.Contains(buf.String(), key) {
			t.Errorf("report is missing %s:\n%s", key, buf.String())
		}
	}
	// Omitted when absent rather than reported as empty noise.
	for _, key := range []string{`"error"`, `"images"`, `"image_errors"`, `"round_usage"`} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("report should omit %s when empty:\n%s", key, buf.String())
		}
	}
}
