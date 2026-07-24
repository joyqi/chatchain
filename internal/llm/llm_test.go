package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEParsing pins the SSE grammar: field split at the first ':', one
// optional leading space stripped, ':' comments skipped, multi-line data
// joined with '\n', dispatch on blank lines, [DONE] sets Done and keeps
// draining, and a final event without a trailing blank line still surfaces.
func TestSSEParsing(t *testing.T) {
	raw := ": comment\n" +
		"event: ping\ndata: {\"a\":1}\n\n" +
		"data: line1\ndata:line2\n\n" +
		"data: [DONE]\n\n" +
		"ignored: field\n"
	s := newSSE(io.NopCloser(strings.NewReader(raw)))

	evt, err := s.Next()
	if err != nil || evt.Type != "ping" || string(evt.Data) != `{"a":1}` {
		t.Fatalf("event 1 = %+v, %v", evt, err)
	}
	evt, err = s.Next()
	if err != nil || evt.Type != "" || string(evt.Data) != "line1\nline2" {
		t.Fatalf("event 2 = %+v, %v (multi-line data join)", evt, err)
	}
	if _, err = s.Next(); err != io.EOF {
		t.Fatalf("expected EOF after [DONE] drain, got %v", err)
	}
	if !s.Done() || !s.SawEvent() {
		t.Fatal("Done/SawEvent not set")
	}

	// No trailing blank line: the last event still dispatches at EOF.
	s2 := newSSE(io.NopCloser(strings.NewReader("data: tail")))
	if evt, err := s2.Next(); err != nil || string(evt.Data) != "tail" {
		t.Fatalf("unterminated final event = %+v, %v", evt, err)
	}
}

// TestRetryPolicy pins SDK-parity retries: 429/5xx retry with Retry-After
// honored, x-should-retry:false stops, 400 never retries, and each attempt
// hits the transport (visible to /debug).
func TestRetryPolicy(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, srv.Client())
	var out map[string]any
	if err := c.Do(context.Background(), "POST", "/x", map[string]any{}, &out); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}

	// 400 is terminal, error is structured, status code lands in Error().
	hits = 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`))
	}))
	defer srv2.Close()
	c2 := New(srv2.URL, srv2.Client())
	err := c2.Do(context.Background(), "POST", "/x", map[string]any{}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 400 || hits != 1 {
		t.Fatalf("400: err=%v hits=%d", err, hits)
	}
	if !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("error string lost status/envelope: %q", err.Error())
	}

	// x-should-retry: false overrides a retryable status.
	hits = 0
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(500)
	}))
	defer srv3.Close()
	c3 := New(srv3.URL, srv3.Client())
	if err := c3.Do(context.Background(), "POST", "/x", map[string]any{}, nil); err == nil || hits != 1 {
		t.Fatalf("x-should-retry:false ignored: err=%v hits=%d", err, hits)
	}
}

// TestStreamCancellation pins the interrupt contract: cancelling the ctx
// mid-stream aborts the body read promptly.
func TestStreamCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-release // hold the stream open
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	c := ChatComp{Client: New(srv.URL, srv.Client())}
	stream, err := c.StreamCompletion(ctx, &ChatCompRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if chunk, err := stream.Next(); err != nil || chunk.Choices[0].Delta.Content != "hi" {
		t.Fatalf("first chunk: %+v, %v", chunk, err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { _, err := stream.Next(); done <- err }()
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatalf("cancel surfaced as %v, want a read error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Next did not return after ctx cancel — interrupt would freeze")
	}
}

// TestNoEventsStream pins the broken-compat-server case: a plain JSON body
// answering a stream request surfaces ErrNoEvents (non-retryable upstream),
// not silent emptiness.
func TestNoEventsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"not a stream"}}]}`))
	}))
	defer srv.Close()
	c := ChatComp{Client: New(srv.URL, srv.Client())}
	stream, err := c.StreamCompletion(context.Background(), &ChatCompRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, err = stream.Next()
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrNoEvents) {
		t.Fatalf("err = %v, want ErrNoEvents", err)
	}
}

// TestInBandStreamError pins mid-stream error events ({"error":...} data).
func TestInBandStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"error\":{\"message\":\"boom\"}}\n\n"))
	}))
	defer srv.Close()
	c := ChatComp{Client: New(srv.URL, srv.Client())}
	stream, err := c.StreamCompletion(context.Background(), &ChatCompRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err = stream.Next(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("in-band error not surfaced: %v", err)
	}
}

// TestGoogleModelPath pins genai tModel parity — including the relay-station
// "vendor/model" convention (zenmux et al.) that maps to
// publishers/{vendor}/models/{model} on Vertex and models/{vendor}/{model}
// on Gemini.
func TestGoogleModelPath(t *testing.T) {
	vertex := Google{Vertex: true, Version: "v1"}
	gemini := Google{Vertex: false, Version: "v1beta"}
	cases := []struct {
		g     Google
		in    string
		want  string
		fails bool
	}{
		{vertex, "gemini-2.5-pro", "/v1/publishers/google/models/gemini-2.5-pro", false},
		{vertex, "bytedance/doubao-seedream-5.0-pro", "/v1/publishers/bytedance/models/doubao-seedream-5.0-pro", false},
		{vertex, "publishers/google/models/x", "/v1/publishers/google/models/x", false},
		{vertex, "models/x", "/v1/models/x", false},
		{vertex, "projects/p/x", "/v1/projects/p/x", false},
		{gemini, "gemini-2.5-pro", "/v1beta/models/gemini-2.5-pro", false},
		{gemini, "vendor/model", "/v1beta/models/vendor/model", false},
		{gemini, "models/x", "/v1beta/models/x", false},
		{vertex, "bad/../escape", "", true},
		{vertex, "bad?x=1", "", true},
	}
	for _, c := range cases {
		got, err := c.g.modelPath(c.in)
		if c.fails {
			if err == nil {
				t.Errorf("modelPath(%q) accepted a path metacharacter", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("modelPath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// TestGoogleVertexModelsFallback pins the relay-station listing fallback: the
// official publisher path 404s (or redirects to an HTML landing page — a
// decode error after redirect-following), and the Gemini-style /v1beta/models
// answer is used instead.
func TestGoogleVertexModelsFallback(t *testing.T) {
	for name, official := range map[string]http.HandlerFunc{
		"404": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		},
		"html-landing": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<!DOCTYPE html><html>landing</html>"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/publishers/google/models":
					official(w, r)
				case "/v1beta/models":
					w.Write([]byte(`{"models":[{"name":"bytedance/doubao"},{"name":"google/gemini-3-pro"}]}`))
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			g := Google{Client: New(srv.URL, srv.Client()), Vertex: true, Version: "v1"}
			names, err := g.Models(context.Background())
			if err != nil || len(names) != 2 || names[0].Name != "bytedance/doubao" {
				t.Fatalf("fallback failed: %v %v", names, err)
			}
		})
	}

	// The official shape, when present, wins (no fallback issued).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/publishers/google/models" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"publisherModels":[{"name":"publishers/google/models/gemini-3-pro"}]}`))
	}))
	defer srv.Close()
	g := Google{Client: New(srv.URL, srv.Client()), Vertex: true, Version: "v1"}
	names, err := g.Models(context.Background())
	if err != nil || len(names) != 1 || names[0].Name != "publishers/google/models/gemini-3-pro" {
		t.Fatalf("official path result = %v %v", names, err)
	}
}
