package chat

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// RequestEntry is one captured HTTP round-trip — the request line and body plus
// the response status and body — browsed by the /debug command. Request fields
// are set once at dispatch; the response fields are filled as the round-trip
// resolves and the (possibly streaming) body is read, guarded by RequestLog.mu.
type RequestEntry struct {
	Time     time.Time     // when the request was sent
	Method   string        // HTTP method
	URL      string        // full request URL
	ReqBody  []byte        // request body, capped at reqLogMaxBody
	Status   string        // response status line ("" until the response arrives)
	Err      string        // transport error, if the round-trip failed
	RespBody []byte        // response body, capped (accumulates while streaming)
	Duration time.Duration // wall time from send to body close
}

const (
	reqLogMaxEntries = 30         // ring size — the most recent N round-trips
	reqLogMaxBody    = 256 * 1024 // per-body capture cap, to bound memory
)

// RequestLog is a bounded, thread-safe ring of recent HTTP round-trips gated by a
// runtime "verbose" switch. Verbose is the /debug Verbose tab's toggle and means
// "record": while off (the default) the transport captures nothing and the
// browser stays empty — no overhead; while on, each round-trip is captured into
// the ring for browsing. Nothing is ever written to the terminal.
type RequestLog struct {
	mu      sync.Mutex
	entries []*RequestEntry // oldest first; capped at reqLogMaxEntries
	verbose atomic.Bool     // recording on/off
}

// NewRequestLog returns an empty log with recording off.
func NewRequestLog() *RequestLog { return &RequestLog{} }

// Verbose reports whether recording is on.
func (l *RequestLog) Verbose() bool { return l.verbose.Load() }

// SetVerbose turns recording on or off. Turning it off leaves already-captured
// entries in place (still browsable); it just stops capturing new ones.
func (l *RequestLog) SetVerbose(v bool) { l.verbose.Store(v) }

// Entries returns a snapshot of the captured round-trips, newest first.
func (l *RequestLog) Entries() []*RequestEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*RequestEntry, len(l.entries))
	for i, e := range l.entries {
		out[len(l.entries)-1-i] = e
	}
	return out
}

// add appends a new entry, evicting the oldest past the ring cap.
func (l *RequestLog) add(e *RequestEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if len(l.entries) > reqLogMaxEntries {
		l.entries = l.entries[len(l.entries)-reqLogMaxEntries:]
	}
}

// HTTPClient returns an *http.Client whose transport records each round-trip
// into the log while recording is on (see Verbose). It never writes to the
// terminal — captured traffic is browsed via /debug.
func (l *RequestLog) HTTPClient() *http.Client {
	return &http.Client{Transport: &recordingTransport{log: l}}
}

// recordingTransport wraps the default transport, capturing each round-trip into
// the RequestLog while recording is on and otherwise passing straight through.
type recordingTransport struct {
	log       *RequestLog
	Transport http.RoundTripper
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	var e *RequestEntry
	if t.log.Verbose() {
		e = &RequestEntry{Time: time.Now(), Method: req.Method, URL: req.URL.String()}
		if req.Body != nil {
			if body, err := io.ReadAll(req.Body); err == nil {
				req.Body = io.NopCloser(bytes.NewReader(body))
				e.ReqBody = capBytes(body)
			}
		}
		t.log.add(e)
	}

	// Upload progress + headers-received phase, driven by the turn's
	// context-injected reporter (progress.go). Attached AFTER the verbose
	// capture above, which reads the body in-process rather than uploading.
	rep := turnProgressFrom(req.Context())
	if rep != nil && req.Body != nil && req.ContentLength > 0 {
		req.Body = &progressBody{rc: req.Body, rep: rep, total: req.ContentLength}
	}

	start := time.Now()
	resp, err := transport.RoundTrip(req)
	rep.sent() // nil-safe: headers received (or the attempt failed)
	if err != nil {
		if e != nil {
			t.log.mu.Lock()
			e.Err = err.Error()
			e.Duration = time.Since(start)
			t.log.mu.Unlock()
		}
		return resp, err
	}

	if e == nil {
		return resp, nil // recording off — capture nothing further
	}
	t.log.mu.Lock()
	e.Status = resp.Status
	t.log.mu.Unlock()
	if resp.Body != nil {
		resp.Body = &recordingBody{rc: resp.Body, log: t.log, entry: e, start: start}
	}
	return resp, nil
}

// recordingBody tees a response body into the entry, accumulating the (capped)
// bytes as they are read — so a streaming SSE response fills in progressively.
type recordingBody struct {
	rc    io.ReadCloser
	log   *RequestLog
	entry *RequestEntry
	start time.Time
}

func (v *recordingBody) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.log.mu.Lock()
		if len(v.entry.RespBody) < reqLogMaxBody {
			v.entry.RespBody = capBytes(append(v.entry.RespBody, p[:n]...))
		}
		v.log.mu.Unlock()
	}
	if err != nil {
		v.log.mu.Lock()
		v.entry.Duration = time.Since(v.start)
		v.log.mu.Unlock()
	}
	return n, err
}

func (v *recordingBody) Close() error {
	// Backstop the duration: the SDK often stops reading an SSE stream at the
	// terminating event without draining to EOF, so Read never sees the error
	// that would set Duration. Fill it in on Close so the row isn't left durationless.
	v.log.mu.Lock()
	if v.entry.Duration == 0 {
		v.entry.Duration = time.Since(v.start)
	}
	v.log.mu.Unlock()
	return v.rc.Close()
}

// capBytes returns b truncated to reqLogMaxBody (copying so the ring never pins
// a larger backing array).
func capBytes(b []byte) []byte {
	if len(b) <= reqLogMaxBody {
		return b
	}
	out := make([]byte, reqLogMaxBody)
	copy(out, b)
	return out
}
