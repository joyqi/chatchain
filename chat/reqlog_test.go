package chat

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestLogCaptures drives a real round-trip through the recording client
// and checks the entry captures the method, URL, request body, status, and
// response body (the latter only after the body is read and closed, as a tee).
func TestRequestLogCaptures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "ping" {
			t.Errorf("server saw body %q, want ping", body)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "pong")
	}))
	defer srv.Close()

	log := NewRequestLog()
	log.SetVerbose(true) // recording must be on to capture
	resp, err := log.HTTPClient().Post(srv.URL+"/v1/thing", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(respBody) != "pong" {
		t.Fatalf("client saw response %q, want pong", respBody)
	}

	entries := log.Entries()
	if len(entries) != 1 {
		t.Fatalf("captured %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Method != "POST" || !strings.HasSuffix(e.URL, "/v1/thing") {
		t.Errorf("entry method/url = %s %s", e.Method, e.URL)
	}
	if string(e.ReqBody) != "ping" {
		t.Errorf("req body = %q, want ping", e.ReqBody)
	}
	if !strings.HasPrefix(e.Status, "201") {
		t.Errorf("status = %q, want 201…", e.Status)
	}
	if string(e.RespBody) != "pong" {
		t.Errorf("resp body = %q, want pong (tee should capture the streamed body)", e.RespBody)
	}
}

// TestRequestLogRecordingOff verifies that with recording off (the default) the
// transport passes through and captures nothing.
func TestRequestLogRecordingOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	log := NewRequestLog() // verbose off by default
	resp, err := log.HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if n := len(log.Entries()); n != 0 {
		t.Errorf("captured %d entries with recording off, want 0", n)
	}
}

// TestRequestLogRingNewestFirst checks the ring caps at reqLogMaxEntries and that
// Entries returns newest-first.
func TestRequestLogRingNewestFirst(t *testing.T) {
	log := NewRequestLog()
	for i := 0; i < reqLogMaxEntries+5; i++ {
		log.add(&RequestEntry{Method: "GET", URL: fmt.Sprintf("/r/%d", i)})
	}
	entries := log.Entries()
	if len(entries) != reqLogMaxEntries {
		t.Fatalf("ring holds %d, want cap %d", len(entries), reqLogMaxEntries)
	}
	// Newest first: the last-added URL leads.
	last := reqLogMaxEntries + 4
	if entries[0].URL != fmt.Sprintf("/r/%d", last) {
		t.Errorf("newest entry = %s, want /r/%d", entries[0].URL, last)
	}
	// Oldest surviving is index 5 (0..4 evicted).
	if entries[len(entries)-1].URL != "/r/5" {
		t.Errorf("oldest surviving = %s, want /r/5", entries[len(entries)-1].URL)
	}
}

// TestRequestLogVerboseToggle covers the runtime switch the /debug Verbose tab
// flips.
func TestRequestLogVerboseToggle(t *testing.T) {
	log := NewRequestLog()
	if log.Verbose() {
		t.Fatal("verbose should default off")
	}
	log.SetVerbose(true)
	if !log.Verbose() {
		t.Fatal("SetVerbose(true) not reflected")
	}
	log.SetVerbose(false)
	if log.Verbose() {
		t.Fatal("SetVerbose(false) not reflected")
	}
}

// TestCapBytes bounds a captured body at reqLogMaxBody.
func TestCapBytes(t *testing.T) {
	small := []byte("hi")
	if got := capBytes(small); string(got) != "hi" {
		t.Errorf("small body altered: %q", got)
	}
	big := make([]byte, reqLogMaxBody+1000)
	if got := capBytes(big); len(got) != reqLogMaxBody {
		t.Errorf("oversized body not capped: len %d, want %d", len(got), reqLogMaxBody)
	}
}
