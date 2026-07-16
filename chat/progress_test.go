package chat

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTurnProgressNilSafety(t *testing.T) {
	if tp := turnProgressFrom(context.Background()); tp != nil {
		t.Fatal("plain context should carry no progress slot")
	}
	var nilTP *turnProgress
	nilTP.send(1, 2) // must not panic
	nilTP.sent()
	nilTP.setHandlers(nil, nil)

	ctx := withTurnProgress(context.Background())
	if turnProgressFrom(ctx) == nil {
		t.Fatal("withTurnProgress slot not retrievable")
	}
}

func TestProgressBodyReports(t *testing.T) {
	rep := &turnProgress{}
	var dones []int64
	var totals []int64
	rep.setHandlers(func(done, total int64) {
		dones = append(dones, done)
		totals = append(totals, total)
	}, nil)

	data := bytes.Repeat([]byte("x"), 10)
	b := &progressBody{rc: io.NopCloser(bytes.NewReader(data)), rep: rep, total: 10}
	if _, err := io.Copy(io.Discard, b); err != nil {
		t.Fatal(err)
	}
	// The final byte always reports, regardless of throttling.
	if len(dones) == 0 || dones[len(dones)-1] != 10 || totals[0] != 10 {
		t.Fatalf("reports = %v/%v, want a final done=10 total=10", dones, totals)
	}

	// Cleared handlers stop deliveries.
	rep.setHandlers(nil, nil)
	rep.send(99, 99)
	if dones[len(dones)-1] != 10 {
		t.Fatal("send after clear still delivered")
	}
}

// fakeTransport answers every request with 200 and records what it saw.
type fakeTransport struct{ gotBody string }

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.gotBody = string(b)
	}
	return &http.Response{Status: "200 OK", StatusCode: 200,
		Body: io.NopCloser(strings.NewReader(""))}, nil
}

// The transport wires the ctx-injected reporter: upload bytes flow through
// progressBody and headers-received fires sent() — with recording off.
func TestRecordingTransportProgress(t *testing.T) {
	rep := &turnProgress{}
	var uploaded int64
	sent := 0
	rep.setHandlers(func(done, total int64) { uploaded = done }, func() { sent++ })
	ctx := context.WithValue(context.Background(), progressKey{}, rep)

	ft := &fakeTransport{}
	rt := &recordingTransport{log: NewRequestLog(), Transport: ft}
	body := strings.Repeat("y", 2048)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://example.test/v1", strings.NewReader(body))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if ft.gotBody != body {
		t.Fatalf("body corrupted through progressBody: %d bytes", len(ft.gotBody))
	}
	if uploaded != int64(len(body)) {
		t.Fatalf("uploaded reported %d, want %d", uploaded, len(body))
	}
	if sent != 1 {
		t.Fatalf("sent() fired %d times, want 1", sent)
	}
}
