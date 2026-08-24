package chat

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chatchain/provider"
	"chatchain/tool"
)

// parallelDispatch is a Dispatcher whose named tools are parallel-capable and
// whose calls block until released, so a test can prove they overlap. When
// byAgent is set the answer comes from the call's "agent" argument instead of
// its name — the delegation shape, where one name covers calls that differ.
type parallelDispatch struct {
	parallel map[string]bool
	byAgent  map[string]bool
	enter    chan struct{} // one token per started call
	release  chan struct{} // closed to let every call finish
	peak     int64
	live     int64
}

// ParallelReporter is an OPTIONAL interface, so a signature that drifts out of
// step here would not fail to compile — it would silently stop being detected
// and serialize everything, passing most of this file while proving nothing.
// The assertion turns that into a build error.
var _ tool.ParallelReporter = (*parallelDispatch)(nil)

func (d *parallelDispatch) Tools() []provider.ToolDef { return nil }

func (d *parallelDispatch) SupportsParallel(name string, args map[string]any) bool {
	if d.byAgent != nil {
		agent, _ := args["agent"].(string)
		return d.byAgent[agent]
	}
	return d.parallel[name]
}

func (d *parallelDispatch) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	n := atomic.AddInt64(&d.live, 1)
	for {
		peak := atomic.LoadInt64(&d.peak)
		if n <= peak || atomic.CompareAndSwapInt64(&d.peak, peak, n) {
			break
		}
	}
	if d.enter != nil {
		d.enter <- struct{}{}
	}
	if d.release != nil {
		<-d.release
	}
	atomic.AddInt64(&d.live, -1)
	return "out:" + name, false, nil
}

func call(id, name string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: map[string]any{}}
}

// parallelRun batches only a RUN of consecutive parallel-capable calls: a
// serial tool separates the ones before it from the ones after, which is what
// keeps a write from overlapping the reads around it.
func TestParallelRunBoundaries(t *testing.T) {
	d := &parallelDispatch{parallel: map[string]bool{"read_file": true, "grep": true}}
	calls := []provider.ToolCall{
		call("1", "read_file"), call("2", "grep"),
		call("3", "edit_file"),
		call("4", "read_file"), call("5", "read_file"), call("6", "read_file"),
	}
	for _, tc := range []struct{ from, want int }{
		{0, 2}, // the leading pair
		{2, 2}, // edit_file batches nothing
		{3, 6}, // the trailing run
		{5, 6},
	} {
		if got := parallelRun(d, calls, tc.from); got != tc.want {
			t.Errorf("parallelRun(from=%d) = %d, want %d", tc.from, got, tc.want)
		}
	}
}

// The same boundaries hold when the calls share a NAME and differ only in
// their arguments — the delegation shape. A per-name answer could not split
// this sequence at all: it would either serialize the fan-out or let the
// write-capable call join a batch.
func TestParallelRunSplitsCallsToOneTool(t *testing.T) {
	d := &parallelDispatch{byAgent: map[string]bool{"search": true, "implement": false}}
	delegate := func(id, agent string) provider.ToolCall {
		return provider.ToolCall{ID: id, Name: "delegate", Arguments: map[string]any{"agent": agent}}
	}
	calls := []provider.ToolCall{
		delegate("1", "search"), delegate("2", "search"),
		delegate("3", "implement"),
		delegate("4", "search"),
	}
	for _, tc := range []struct{ from, want int }{
		{0, 2}, // the two searches batch
		{2, 2}, // the write-capable one runs alone
		{3, 4}, // and the search after it batches again
	} {
		if got := parallelRun(d, calls, tc.from); got != tc.want {
			t.Errorf("parallelRun(from=%d) = %d, want %d", tc.from, got, tc.want)
		}
	}
}

// The calls in a batch really do overlap — the point of the change.
func TestParallelBatchRunsConcurrently(t *testing.T) {
	const n = 4
	d := &parallelDispatch{
		parallel: map[string]bool{"read_file": true},
		enter:    make(chan struct{}, n),
		release:  make(chan struct{}),
	}
	calls := make([]provider.ToolCall, n)
	for i := range calls {
		calls[i] = call(string(rune('a'+i)), "read_file")
	}

	done := make(chan []provider.Message, 1)
	go func() {
		msgs, _ := runParallelBatch(context.Background(), nil, newTranscript(&recSurface{}, nil), d, calls)
		done <- msgs
	}()

	// Every call must start before any is allowed to finish; serial
	// execution would deadlock here, which is the assertion.
	for i := 0; i < n; i++ {
		select {
		case <-d.enter:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d calls started — the batch ran serially", i, n)
		}
	}
	close(d.release)
	<-done

	if peak := atomic.LoadInt64(&d.peak); peak != n {
		t.Fatalf("peak concurrency = %d, want %d", peak, n)
	}
}

// Results answer their calls in CALL order however the calls finish — the
// protocol requires each result to correspond to its call, and the rendered
// rows must describe the same sequence the history records.
func TestParallelBatchKeepsCallOrder(t *testing.T) {
	d := &parallelDispatch{parallel: map[string]bool{"read_file": true, "grep": true}}
	calls := []provider.ToolCall{call("c1", "grep"), call("c2", "read_file"), call("c3", "grep")}

	s := &recSurface{}
	msgs, err := runParallelBatch(context.Background(), nil, newTranscript(s, nil), d, calls)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(calls) {
		t.Fatalf("got %d results for %d calls", len(msgs), len(calls))
	}
	for i, m := range msgs {
		if m.ToolCallID != calls[i].ID || m.ToolCallName != calls[i].Name {
			t.Fatalf("result %d = %s/%s, want %s/%s", i, m.ToolCallID, m.ToolCallName, calls[i].ID, calls[i].Name)
		}
		if m.Role != "tool" {
			t.Fatalf("result %d has role %q", i, m.Role)
		}
	}
	// The event rows follow the same order.
	var seen []string
	for _, e := range s.events {
		if strings.HasPrefix(e, "line:") {
			seen = append(seen, e)
		}
	}
	if len(seen) != len(calls) {
		t.Fatalf("got %d event rows for %d calls: %v", len(seen), len(calls), seen)
	}
	for i, tc := range calls {
		if !strings.Contains(seen[i], tc.Name) {
			t.Fatalf("event row %d = %q, want the %s call", i, seen[i], tc.Name)
		}
	}
}

// A cancelled batch still returns a result for every call it made: a call
// without a result would leave the round's history unable to answer itself.
func TestParallelBatchCancelledStillAnswersEveryCall(t *testing.T) {
	d := &parallelDispatch{parallel: map[string]bool{"read_file": true}}
	calls := []provider.ToolCall{call("a", "read_file"), call("b", "read_file")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msgs, err := runParallelBatch(ctx, nil, newTranscript(&recSurface{}, nil), d, calls)
	if err != errInterrupted {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if len(msgs) != len(calls) {
		t.Fatalf("cancelled batch returned %d results for %d calls", len(msgs), len(calls))
	}
}

// A dispatcher without the capability serializes everything, so an existing
// setup behaves exactly as before.
func TestParallelRunNeedsTheCapability(t *testing.T) {
	var plain tool.Dispatcher = &noCapDispatch{}
	calls := []provider.ToolCall{call("1", "read_file"), call("2", "read_file")}
	if got := parallelRun(plain, calls, 0); got != 0 {
		t.Fatalf("parallelRun = %d, want 0 without the capability", got)
	}
}

type noCapDispatch struct{}

func (noCapDispatch) Tools() []provider.ToolDef { return nil }
func (noCapDispatch) CallTool(context.Context, string, map[string]any) (string, bool, error) {
	return "", false, nil
}

// The quiet (-m) loop batches too. It had no parallelism at all: the
// machinery took a transcript, so only the interactive path could reach it,
// and a scripted run — CI, a pipeline, a parent treating this binary as a
// child — read four files one at a time for no reason.
func TestQuietLoopBatchesParallelCalls(t *testing.T) {
	d := &parallelDispatch{
		parallel: map[string]bool{"read_file": true},
		enter:    make(chan struct{}, 4),
		release:  make(chan struct{}),
	}
	tp := &batchingProvider{calls: []provider.ToolCall{
		call("1", "read_file"), call("2", "read_file"), call("3", "read_file"),
	}}
	// Release only once every call has started: if the loop were serial the
	// second would never start and this would deadlock into the test timeout.
	go func() {
		for i := 0; i < 3; i++ {
			<-d.enter
		}
		close(d.release)
	}()
	history := []provider.Message{{Role: "user", Content: "go"}}
	reply, _, err := executeWithTools(context.Background(), tp, d, &history,
		nil, "", 0, quietHost{rec: newRunRecorder()})
	if err != nil {
		t.Fatalf("quiet loop failed: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q", reply)
	}
	if d.peak < 3 {
		t.Errorf("peak concurrency = %d, want 3 — the calls did not overlap", d.peak)
	}
	// Results still answer their calls in call order, batch or not.
	var ids []string
	for _, m := range history {
		if m.Role == "tool" {
			ids = append(ids, m.ToolCallID)
		}
	}
	if strings.Join(ids, ",") != "1,2,3" {
		t.Errorf("tool results in %v, want call order", ids)
	}
}

// batchingProvider asks for a fixed set of calls once, then answers.
type batchingProvider struct {
	calls []provider.ToolCall
	round int
}

func (p *batchingProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.round++
	if p.round == 1 {
		return "", "", p.calls, nil
	}
	return "done", "", nil, nil
}
