package chat

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"chatchain/provider"
)

func TestLastRounds(t *testing.T) {
	u := func(s string) provider.Message { return provider.Message{Role: "user", Content: s} }
	a := func(s string) provider.Message { return provider.Message{Role: "assistant", Content: s} }
	sys := provider.Message{Role: "system", Content: "sys"}
	tool := provider.Message{Role: "tool", Content: "result"}

	tests := []struct {
		name    string
		history []provider.Message
		n       int
		want    []provider.Message
	}{
		{"empty", nil, 3, nil},
		{"zero-rounds", []provider.Message{u("q"), a("r")}, 0, nil},
		{
			"fewer-than-n-returns-all",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2")},
			3,
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2")},
		},
		{
			"exactly-n",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3")},
			3,
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3")},
		},
		{
			"more-than-n-keeps-last-n-starting-at-user",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3"), u("q4"), a("r4")},
			3,
			[]provider.Message{u("q2"), a("r2"), u("q3"), a("r3"), u("q4"), a("r4")},
		},
		{
			"system-excluded",
			[]provider.Message{sys, u("q1"), a("r1")},
			3,
			[]provider.Message{u("q1"), a("r1")},
		},
		{
			"trailing-tool-results-kept",
			[]provider.Message{u("q1"), a("r1"), u("q2"), {Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}}, tool, tool},
			1,
			[]provider.Message{u("q2"), {Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}}, tool, tool},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastRounds(tt.history, tt.n); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lastRounds(..., %d) = %#v, want %#v", tt.n, got, tt.want)
			}
		})
	}
}

func TestEchoRounds(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "first question", Attachments: []provider.Attachment{{Filename: "a.png"}, {Filename: "b.pdf"}}},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "bash"}}},
		{Role: "tool", Content: "output 1"},
		{Role: "tool", Content: "output 2"},
		{Role: "assistant", Content: "final answer", Reasoning: "secret thinking"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "partial reply", Interrupted: true},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs)
	out := buf.String()

	for _, want := range []string{
		"first question",
		"(2 attachment(s))",
		"⚙ 2 tool call(s)",
		"final answer",
		"second question",
		"partial reply",
		"(interrupted)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("echoRounds output missing %q\noutput:\n%s", want, out)
		}
	}
	// Tool bodies and reasoning must not be replayed.
	for _, forbidden := range []string{"output 1", "output 2", "secret thinking"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("echoRounds output leaked %q\noutput:\n%s", forbidden, out)
		}
	}
	// The tool line must precede the round's final reply.
	if strings.Index(out, "⚙ 2 tool call(s)") > strings.Index(out, "final answer") {
		t.Errorf("tool call line should precede the round's reply\noutput:\n%s", out)
	}
}

func TestEchoRoundsTrailingToolResults(t *testing.T) {
	// A history ending in tool results (no trailing assistant reply) still gets
	// its aggregated tool line at the end of the replay.
	msgs := []provider.Message{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}},
		{Role: "tool", Content: "out"},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs)
	if !strings.Contains(buf.String(), "⚙ 1 tool call(s)") {
		t.Errorf("expected trailing tool call line, got:\n%s", buf.String())
	}
}
