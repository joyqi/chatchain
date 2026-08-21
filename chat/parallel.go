package chat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"chatchain/provider"
	"chatchain/tool"
)

// Parallel tool execution. A round's calls arrive as a list, and the ones
// declared safe to run concurrently (tool.ParallelReporter — today the
// read-only file tools) execute together instead of one after another. The
// declaration is per CALL: a tool whose calls differ in kind answers per
// call, so the batches follow what is actually safe rather than what a tool
// name can promise.
//
// Two orderings are kept regardless of who finishes first:
//
//   - RESULTS go into history in call order. That is a protocol requirement,
//     not a preference: a round's tool results must correspond to its calls.
//   - EVENT ROWS are rendered in call order too, after the batch completes.
//     Rendering by completion order would shuffle the transcript against the
//     history it describes for no gain the reader can use.
//
// Everything the serial path does — approval gates, interactive surfaces,
// expanded diffs — is absent here by construction: a tool may only opt into
// parallel execution if it needs none of them (see tool.parallelizer).

// parallelRun returns the end of the run of consecutive parallel-capable
// calls starting at i (i itself when the call at i is not one).
//
// Capability is asked per CALL, not per tool name, so a round that mixes
// concurrent-safe and serial calls to the SAME tool splits at the boundaries
// without any special handling: [a a b a] batches [a a], runs b alone, then
// [a]. That falls out of scanning for consecutive runs — and it drops the
// serial one into the path that already has approval gates, surfaces and
// expanded rendering, which is exactly where a call needing any of them
// belongs.
func parallelRun(dispatch tool.Dispatcher, calls []provider.ToolCall, i int) int {
	j := i
	for j < len(calls) && supportsParallel(dispatch, calls[j]) {
		j++
	}
	return j
}

// batchOutcome is one call's result, kept beside its index so the batch can
// be reassembled in call order.
type batchOutcome struct {
	text    string
	isError bool
	dur     time.Duration
}

// runParallelBatch executes calls concurrently and returns their results in
// CALL order. The returned error is errInterrupted when the user cancelled;
// the results gathered so far are still returned, because the calls that did
// complete already had their effects and their results must answer their
// calls.
//
// pushScope is the interrupt seam (ui.UI.PushCancelScope) rather than the UI
// itself: the batch needs exactly one thing from the terminal — somewhere to
// register the cancel that ESC fires — and taking that alone is what lets a
// test drive this without a Program.
func runParallelBatch(ctx context.Context, pushScope func(context.CancelFunc) func(), tr *transcript, dispatch tool.Dispatcher, calls []provider.ToolCall) ([]provider.Message, error) {
	headers := make([]string, len(calls))
	for i, tc := range calls {
		headers[i] = CodeStyle.Sprint(toolCallHeader(dispatch, tc))
	}
	// One widget for the whole batch, opened on the first call's header —
	// the same shape a single call raises. finishCall relabels it to
	// "Working…" as the rows land, so the batch needs no label of its own.
	tr.openCall(headers[0])

	// One cancel scope for the batch: ESC cancels the run, not one member of
	// it, and a half-cancelled batch would leave calls without results.
	batchCtx, cancel := context.WithCancel(ctx)
	pop := func() {}
	if pushScope != nil {
		pop = pushScope(cancel)
	}

	outcomes := make([]batchOutcome, len(calls))
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc provider.ToolCall) {
			defer wg.Done()
			started := time.Now()
			text, isError, err := dispatch.CallTool(batchCtx, tc.Name, tc.Arguments)
			if err != nil {
				text, isError = fmt.Sprintf("Error calling tool: %v", err), true
			}
			outcomes[i] = batchOutcome{text: text, isError: isError, dur: time.Since(started)}
		}(i, tc)
	}
	wg.Wait()
	pop()
	cancel()

	msgs := make([]provider.Message, 0, len(calls))
	for i, tc := range calls {
		tr.finishCall(headers[i], outcomes[i].text, outcomes[i].isError, outcomes[i].dur)
		msgs = append(msgs, provider.Message{
			Role:         "tool",
			Content:      outcomes[i].text,
			ToolCallID:   tc.ID,
			ToolCallName: tc.Name,
			IsError:      outcomes[i].isError,
		})
	}
	if ctx.Err() != nil {
		return msgs, errInterrupted
	}
	return msgs, nil
}
