package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
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

	_, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history, noopDispatcher{}.Tools(), "", limit, quietHost{rec: newRunRecorder()})
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
	reply, _, err := executeWithTools(context.Background(), tp, noopDispatcher{}, &history, noopDispatcher{}.Tools(), "", 0, quietHost{rec: newRunRecorder()})
	if err != nil {
		t.Fatalf("unlimited loop errored: %v", err)
	}
	if tp.calls != 76 || reply != "done" {
		t.Fatalf("calls = %d reply = %q, want 76 rounds then the final answer", tp.calls, reply)
	}
}

// growingDispatcher gains a tool after its search_tools is called — the
// deferred-loading shape.
type growingDispatcher struct{ loaded bool }

func (g *growingDispatcher) Tools() []provider.ToolDef {
	defs := []provider.ToolDef{{Name: "search_tools"}}
	if g.loaded {
		defs = append(defs, provider.ToolDef{Name: "late_tool"})
	}
	return defs
}
func (g *growingDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name == "search_tools" {
		g.loaded = true
		return "Loaded 1 tool(s)", false, nil
	}
	return "ok", false, nil
}

// searchingToolProvider calls search_tools in round 1 and records the tool
// set each round advertises.
type searchingToolProvider struct {
	rounds   int
	perRound [][]string
}

func (p *searchingToolProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	var names []string
	for _, d := range tools {
		names = append(names, d.Name)
	}
	p.perRound = append(p.perRound, names)
	p.rounds++
	if p.rounds == 1 {
		return "", "", []provider.ToolCall{{ID: "c1", Name: "search_tools", Arguments: map[string]any{"query": "late"}}}, nil
	}
	return "done", "", nil, nil
}

// The Once loop re-queries the dispatcher every round: a tool loaded by a
// search_tools call must be advertised in the very next request.
func TestExecuteWithToolsRefreshesPerRound(t *testing.T) {
	tp := &searchingToolProvider{}
	dispatch := &growingDispatcher{}
	history := []provider.Message{{Role: "user", Content: "go"}}

	reply, _, err := executeWithTools(context.Background(), tp, dispatch, &history, dispatch.Tools(), "", 0, quietHost{rec: newRunRecorder()})
	if err != nil || reply != "done" {
		t.Fatalf("loop failed: %q %v", reply, err)
	}
	if len(tp.perRound) != 2 {
		t.Fatalf("rounds = %d, want 2", len(tp.perRound))
	}
	round2 := strings.Join(tp.perRound[1], ",")
	if !strings.Contains(round2, "late_tool") {
		t.Fatalf("round 2 must advertise the searched-in tool, got %v", tp.perRound[1])
	}
}
