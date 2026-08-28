package chat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/joyqi/iota/provider"
)

// retryRound contract: a transient failure re-issues the SAME call — the round
// closure — and nothing else. The old turn-level replay re-executed completed
// tool calls; these tests pin the replacement's edges.

func compressBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = old })
}

func stubBusy(count *int) func(string) func() {
	return func(string) func() { *count++; return func() {} }
}

func TestRetryRoundRecoversAndNotices(t *testing.T) {
	compressBackoff(t)
	calls, busies := 0, 0
	var notices []string
	content, _, _, err := retryRound(context.Background(), stubBusy(&busies),
		func(f string, a ...any) { notices = append(notices, fmt.Sprintf(f, a...)) },
		true, func() (string, string, []provider.ToolCall, error) {
			calls++
			if calls < 3 {
				return "", "", nil, errors.New("received error while streaming: overloaded_error")
			}
			return "done", "", nil, nil
		})
	if err != nil || content != "done" {
		t.Fatalf("want recovery, got content=%q err=%v", content, err)
	}
	if calls != 3 || busies != 2 {
		t.Fatalf("want 3 calls / 2 busy rows, got %d / %d", calls, busies)
	}
	if len(notices) != 1 {
		t.Fatalf("want exactly one recovery notice, got %v", notices)
	}
}

func TestRetryRoundPermanentErrorNotRetried(t *testing.T) {
	compressBackoff(t)
	calls := 0
	_, _, _, err := retryRound(context.Background(), stubBusy(new(int)),
		func(string, ...any) {}, true,
		func() (string, string, []provider.ToolCall, error) {
			calls++
			return "", "", nil, errors.New("unexpected status 400: bad request")
		})
	if err == nil || calls != 1 {
		t.Fatalf("a 4xx must surface on the first call, got calls=%d err=%v", calls, err)
	}
}

func TestRetryRoundDisallowedPassesThrough(t *testing.T) {
	compressBackoff(t)
	calls := 0
	_, _, _, err := retryRound(context.Background(), stubBusy(new(int)),
		func(string, ...any) {}, false,
		func() (string, string, []provider.ToolCall, error) {
			calls++
			return "", "", nil, errors.New("received error while streaming: overloaded_error")
		})
	if err == nil || calls != 1 {
		t.Fatalf("allowed=false must not retry, got calls=%d err=%v", calls, err)
	}
}

func TestRetryRoundInterruptedDuringBackoff(t *testing.T) {
	old := retryBackoff
	retryBackoff = time.Hour // the cancelled ctx must win, never the timer
	t.Cleanup(func() { retryBackoff = old })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, _, _, err := retryRound(ctx, stubBusy(new(int)),
		func(string, ...any) {}, true,
		func() (string, string, []provider.ToolCall, error) {
			calls++
			return "", "", nil, errors.New("received error while streaming: overloaded_error")
		})
	if !errors.Is(err, errInterrupted) || calls != 1 {
		t.Fatalf("want errInterrupted after 1 call, got calls=%d err=%v", calls, err)
	}
}
