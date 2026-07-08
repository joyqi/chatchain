package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"chatchain/provider"
)

// loopingToolProvider always requests another tool call, never producing a
// final text response — the runaway case maxToolRounds guards against.
type loopingToolProvider struct{ calls int }

func (p *loopingToolProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.calls++
	return "", "", []provider.ToolCall{{ID: fmt.Sprintf("call-%d", p.calls), Name: "noop", Arguments: map[string]any{}}}, nil
}

// noopDispatcher answers every tool call successfully.
type noopDispatcher struct{}

func (noopDispatcher) Tools() []provider.ToolDef { return []provider.ToolDef{{Name: "noop"}} }
func (noopDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	return "ok", false, nil
}

func TestToolLoopCap(t *testing.T) {
	tp := &loopingToolProvider{}
	history := []provider.Message{{Role: "user", Content: "go"}}

	_, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history, noopDispatcher{}.Tools(), "", io.Discard, true)
	if !errors.Is(err, errToolRoundsExceeded) {
		t.Fatalf("err = %v, want errToolRoundsExceeded", err)
	}
	if tp.calls != maxToolRounds {
		t.Errorf("model calls = %d, want exactly %d", tp.calls, maxToolRounds)
	}
	if isRetryable(err) {
		t.Error("the loop-cap error must not be retried")
	}

	// History stays well-formed: the user message followed by complete
	// assistant/tool round pairs — every tool call has its matching result.
	if want := 1 + 2*maxToolRounds; len(history) != want {
		t.Fatalf("len(history) = %d, want %d", len(history), want)
	}
	if history[0].Role != "user" {
		t.Fatalf("history[0].Role = %q, want user", history[0].Role)
	}
	for i := 1; i < len(history); i += 2 {
		a, r := history[i], history[i+1]
		if a.Role != "assistant" || len(a.ToolCalls) != 1 {
			t.Fatalf("history[%d] = %+v, want an assistant message with one tool call", i, a)
		}
		if r.Role != "tool" || r.ToolCallID != a.ToolCalls[0].ID || r.IsError {
			t.Fatalf("history[%d] = %+v, want the matching successful tool result", i+1, r)
		}
	}
}
