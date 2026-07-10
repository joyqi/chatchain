package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"
)

// TestActionFromURL maps API endpoints to short action names, provider-agnostic
// (Chat is checked before Models because Gemini's generateContent path also
// contains "/models").
func TestActionFromURL(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com/v1/messages":                                          "Chat",
		"https://api.openai.com/v1/chat/completions":                                     "Chat",
		"https://api.openai.com/v1/responses":                                            "Chat",
		"https://generativelanguage.googleapis.com/v1beta/models/gemini:generateContent": "Chat",
		"https://api.openai.com/v1/models":                                               "Models",
		"https://generativelanguage.googleapis.com/v1beta/models":                        "Models",
	}
	for url, want := range cases {
		if got := actionFromURL(url); got != want {
			t.Errorf("actionFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestLastUserText digs the most recent user message out of the various provider
// body shapes (OpenAI/Anthropic messages, OpenAI Responses input, Gemini
// contents), content-as-array parts, and returns "" for a bodyless request.
func TestLastUserText(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"anthropic", `{"messages":[{"role":"user","content":"你好"}]}`, "你好"},
		{"openai last user", `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"}]}`, "hello"},
		{"content parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"pic"}]}]}`, "pic"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"explain"}]}]}`, "explain"},
		{"responses input", `{"input":"do it"}`, "do it"},
		{"last user wins", `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]}`, "second"},
		{"no body", ``, ""},
		{"model listing", `{}`, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastUserText([]byte(tt.body)); got != tt.want {
				t.Errorf("lastUserText = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRequestRowsStyled checks a row carries the action, the summary, the time,
// and the status — with STYLING (ANSI) and no raw method/URL.
func TestRequestRowsStyled(t *testing.T) {
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = true })
	at := time.Date(2026, 7, 10, 15, 4, 5, 0, time.UTC)
	e := &RequestEntry{
		Time:     at,
		Method:   "POST",
		URL:      "https://api.anthropic.com/v1/messages",
		ReqBody:  []byte(`{"messages":[{"role":"user","content":"你好世界"}]}`),
		Status:   "200 OK",
		Duration: 1200 * time.Millisecond,
	}
	row := requestRows([]*RequestEntry{e})[0]
	if !strings.Contains(row, "\x1b[") {
		t.Errorf("row is not styled (no ANSI): %q", row)
	}
	plain := stripANSI(row)
	for _, want := range []string{"15:04:05", "Chat", "你好世界", "200", "1.2s"} {
		if !strings.Contains(plain, want) {
			t.Errorf("row (plain %q) missing %q", plain, want)
		}
	}
	if strings.Contains(plain, "POST") || strings.Contains(plain, "/v1/messages") {
		t.Errorf("row leaked raw method/URL: %q", plain)
	}
}

// TestStatusColOutcome checks the status column is colored by outcome, and a
// model listing (no user text) shows the "—" placeholder in the summary.
func TestStatusColOutcome(t *testing.T) {
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = true })
	ok := statusCol(&RequestEntry{Status: "200 OK"})
	bad := statusCol(&RequestEntry{Status: "401 Unauthorized"})
	if !strings.Contains(ok, "200") || !strings.Contains(bad, "401") {
		t.Fatalf("status text missing: %q / %q", ok, bad)
	}
	if stripANSI(ok) == ok || stripANSI(bad) == bad {
		t.Errorf("status codes should be colored (carry ANSI): %q / %q", ok, bad)
	}
	if ok == bad {
		t.Errorf("2xx and 4xx should differ in style")
	}
	sum := stripANSI(summaryCol(&RequestEntry{URL: "https://api.openai.com/v1/models"}))
	if !strings.HasPrefix(strings.TrimSpace(sum), "—") {
		t.Errorf("empty summary should be the — placeholder, got %q", sum)
	}
}

// TestIsReadOnlyViewer guards the fix for the "/debug is slow right after the
// first chat" bug: read-only viewer commands must skip the titleWG.Wait() that
// would otherwise block them on the ~1s async title-generation request.
func TestIsReadOnlyViewer(t *testing.T) {
	for _, in := range []string{"/debug", "/status", "/tools", "/skills", "/tools foo"} {
		if !isReadOnlyViewer(in) {
			t.Errorf("isReadOnlyViewer(%q) = false, want true", in)
		}
	}
	// Provider-touching / mutating commands (and plain messages) must still wait.
	for _, in := range []string{"/model", "/compact", "/session", "/file", "你好", "/debugx"} {
		if isReadOnlyViewer(in) {
			t.Errorf("isReadOnlyViewer(%q) = true, want false", in)
		}
	}
}
