package chat

import (
	"context"
	"io"
	"sync"
	"time"
)

// Turn progress: the HTTP transport (reqlog.go) reports request-upload
// progress and the headers-received moment through a context-injected
// turnProgress; the streaming loops (run.go) install handlers that drive the
// status line's phase widget. Providers pass their ctx into internal/llm
// requests, so the value rides req.Context() with no interface changes.

// sendProgressMinBytes: uploads smaller than this never surface the "Sending
// request" phase — only attachment-sized bodies are worth narrating.
const sendProgressMinBytes = 128 * 1024

type progressKey struct{}

// turnProgress is a mutable handler slot: the turn installs it into its ctx
// once, and each streaming call sets/clears the handlers around its run.
type turnProgress struct {
	mu     sync.Mutex
	onSend func(done, total int64)
	onSent func()
}

// withTurnProgress installs a fresh turnProgress into ctx.
func withTurnProgress(ctx context.Context) context.Context {
	return context.WithValue(ctx, progressKey{}, &turnProgress{})
}

// turnProgressFrom extracts the turn's progress slot; nil when absent
// (non-interactive runs, background calls like titles and compaction).
func turnProgressFrom(ctx context.Context) *turnProgress {
	tp, _ := ctx.Value(progressKey{}).(*turnProgress)
	return tp
}

// setHandlers installs the phase callbacks; clear with (nil, nil).
func (t *turnProgress) setHandlers(onSend func(done, total int64), onSent func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onSend, t.onSent = onSend, onSent
	t.mu.Unlock()
}

func (t *turnProgress) send(done, total int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	fn := t.onSend
	t.mu.Unlock()
	if fn != nil {
		fn(done, total)
	}
}

func (t *turnProgress) sent() {
	if t == nil {
		return
	}
	t.mu.Lock()
	fn := t.onSent
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// progressBody counts a request body as the transport uploads it, reporting
// throttled progress (and always the final byte).
type progressBody struct {
	rc    io.ReadCloser
	rep   *turnProgress
	done  int64
	total int64
	last  time.Time
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.done += int64(n)
		if b.done == b.total || time.Since(b.last) >= 150*time.Millisecond {
			b.last = time.Now()
			b.rep.send(b.done, b.total)
		}
	}
	return n, err
}

func (b *progressBody) Close() error { return b.rc.Close() }
