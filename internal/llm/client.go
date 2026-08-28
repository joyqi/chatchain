// Package llm is the minimal wire client behind provider/: hand-rolled HTTP +
// SSE for the few endpoints iota actually uses, replacing the official
// SDKs (docs/design/internal-llm-client.md). One dialect file per wire shape
// (chatcomp, responses, anthropic, google); provider/ maps provider.Message to
// and from the dialect structs.
//
// Contracts the whole package must keep (they are load-bearing upstream):
//   - every request binds ctx (interrupt = ctx cancel aborting the body read);
//   - streaming deltas surface incrementally (they are the interrupt partials);
//   - non-2xx becomes *StatusError with the numeric code in Error() (retry
//     classification and /debug display);
//   - every request goes through the injected *http.Client (request logging).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusError is a non-2xx API response. Error() embeds the numeric status
// code and the response's error envelope — the same shape the SDKs produced —
// so display stays informative and retry classification can type-assert.
type StatusError struct {
	Status     int
	StatusText string
	Method     string
	URL        string
	Body       string // the top-level "error" JSON when present, else a body snippet
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %q: %d %s %s", e.Method, e.URL, e.Status, e.StatusText, e.Body)
}

// ErrNoEvents reports a streaming request whose response carried no SSE
// events at all — typically an OpenAI-compatible server answering a
// stream:true request with a plain JSON body. Non-retryable.
var ErrNoEvents = errors.New("stream ended without any SSE events (server did not stream?)")

// Client is one provider endpoint: base URL, static headers, retry policy,
// and the transport. Dialect types embed it.
type Client struct {
	HTTP    *http.Client
	BaseURL string      // normalized without a trailing slash
	Header  http.Header // static per-request headers (auth key, api versions)
	// Auth, when set, is called per attempt to authorize the request
	// (Vertex Bearer tokens with refresh); static keys just use Header.
	Auth    func(ctx context.Context, req *http.Request) error
	Retries int // extra attempts after the first (SDK default parity: 2)
}

// New builds a Client over the caller's http.Client. A nil client falls back
// to a DefaultTransport clone (keeps HTTPS_PROXY and HTTP/2) with a header
// timeout; Client.Timeout is never set — it would kill long SSE streams.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			t := tr.Clone()
			t.ResponseHeaderTimeout = 2 * time.Minute
			hc = &http.Client{Transport: t}
		} else {
			hc = &http.Client{}
		}
	}
	return &Client{
		HTTP:    hc,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Header:  http.Header{},
		Retries: 2,
	}
}

// Do sends a JSON request and decodes the 2xx response body into out
// (nil out discards it).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream sends a JSON request and returns the response as an SSE event
// stream. Retries apply only up to the first successful response; an errored
// stream is never resumed. The caller must Close the stream.
func (c *Client) Stream(ctx context.Context, method, path string, body any) (*SSE, error) {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return newSSE(resp.Body), nil
}

// send runs the request with the retry policy: transport errors and
// 408/409/429/≥500 retry (x-should-retry overrides), Retry-After[-Ms] is
// honored, otherwise exponential backoff 0.5s·2ⁿ capped at 8s minus up to 25%
// jitter — SDK parity, sitting ABOVE the injected client so every attempt is
// visible to /debug.
func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("llm: encode request: %w", err)
		}
	}
	url := c.BaseURL + path

	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, vs := range c.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if c.Auth != nil {
			if err := c.Auth(ctx, req); err != nil {
				return nil, fmt.Errorf("llm: authorize request: %w", err)
			}
		}

		resp, err := c.HTTP.Do(req)
		if err == nil && resp.StatusCode < 400 {
			return resp, nil
		}

		var final error
		if err != nil {
			final = err
		} else {
			final = newStatusError(req, resp) // reads and closes the body
		}
		if attempt >= c.Retries || ctx.Err() != nil || !shouldRetry(err, resp) {
			return nil, final
		}
		t := time.NewTimer(retryDelay(resp, attempt))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func shouldRetry(err error, resp *http.Response) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	switch resp.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return resp.StatusCode >= 500
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if ms := resp.Header.Get("retry-after-ms"); ms != "" {
			if n, err := strconv.Atoi(ms); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
		}
		if ra := resp.Header.Get("retry-after"); ra != "" {
			if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
			if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 {
					return d
				}
			}
		}
	}
	d := 500 * time.Millisecond << attempt
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	// Uniform jitter: sleep in [0.75d, d].
	return d - time.Duration(rand.Int63n(int64(d)/4+1))
}

// newStatusError shapes a non-2xx response: the top-level "error" JSON value
// when the body carries one (openai/anthropic/google envelopes), else a
// truncated body snippet. Always closes the body.
func newStatusError(req *http.Request, resp *http.Response) *StatusError {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	detail := strings.TrimSpace(string(raw))
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Error) > 0 {
		detail = string(envelope.Error)
	}
	if len(detail) > 2048 {
		detail = detail[:2048] + "…"
	}
	return &StatusError{
		Status:     resp.StatusCode,
		StatusText: http.StatusText(resp.StatusCode),
		Method:     req.Method,
		URL:        req.URL.String(),
		Body:       detail,
	}
}
