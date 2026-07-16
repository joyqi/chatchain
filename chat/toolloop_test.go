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
// loopingToolProvider keeps requesting tool calls; with stopAfter > 0 it
// answers normally on round stopAfter+1 (an eventually-finishing agent).
type loopingToolProvider struct {
	calls     int
	stopAfter int
}

func (p *loopingToolProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.calls++
	if p.stopAfter > 0 && p.calls > p.stopAfter {
		return "done", "", nil, nil
	}
	return "", "", []provider.ToolCall{{ID: fmt.Sprintf("call-%d", p.calls), Name: "noop", Arguments: map[string]any{}}}, nil
}

// noopDispatcher answers every tool call successfully.
type noopDispatcher struct{}

func (noopDispatcher) Tools() []provider.ToolDef { return []provider.ToolDef{{Name: "noop"}} }
func (noopDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	return "ok", false, nil
}

func TestToolLoopCap(t *testing.T) {
	// Opt-in limit (--max-turns): the loop stops after exactly N rounds.
	const limit = 7
	tp := &loopingToolProvider{}
	history := []provider.Message{{Role: "user", Content: "go"}}

	_, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history, noopDispatcher{}.Tools(), "", limit)
	if !errors.Is(err, errToolRoundsExceeded) {
		t.Fatalf("err = %v, want errToolRoundsExceeded", err)
	}
	if tp.calls != limit {
		t.Errorf("model calls = %d, want exactly %d", tp.calls, limit)
	}
	if isRetryable(err) {
		t.Error("the loop-cap error must not be retried")
	}

	// History stays well-formed: the user message followed by complete
	// assistant/tool round pairs — every tool call has its matching result.
	if want := 1 + 2*limit; len(history) != want {
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

// TestToolLoopUnlimitedByDefault pins the no-default-cap contract: with
// maxTurns 0 the loop runs past any historical cap and ends only when the
// model stops calling tools.
func TestToolLoopUnlimitedByDefault(t *testing.T) {
	tp := &loopingToolProvider{stopAfter: 75}
	history := []provider.Message{{Role: "user", Content: "go"}}
	reply, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history, noopDispatcher{}.Tools(), "", 0)
	if err != nil {
		t.Fatalf("unlimited loop errored: %v", err)
	}
	if tp.calls != 76 || reply != "done" {
		t.Fatalf("calls = %d reply = %q, want 76 rounds then the final answer", tp.calls, reply)
	}
}
