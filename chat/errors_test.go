package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/joyqi/iota/internal/llm"
	"github.com/joyqi/iota/provider"
)

func statusErr(status int, body string) *llm.StatusError {
	return &llm.StatusError{Status: status, StatusText: "Status Text", Method: "POST", URL: "https://api.example.com/v1/x", Body: body}
}

// describeError turns wire errors into a status-class headline plus the
// envelope's message — never the raw URL/JSON dump — and attaches an
// actionable hint where one exists.
func TestDescribeError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		headline string
		detail   string // "" = expect no detail rows
		hint     string
	}{
		{
			name:     "rate limit with openai envelope",
			err:      statusErr(429, `{"message":"Rate limit reached for gpt-4o","type":"tokens","code":"rate_limit_exceeded"}`),
			headline: "Rate limited (429)",
			detail:   "Rate limit reached for gpt-4o",
		},
		{
			name:     "auth failure hints at the key",
			err:      statusErr(401, `{"message":"Incorrect API key provided"}`),
			headline: "Authentication failed (401)",
			detail:   "Incorrect API key provided",
			hint:     "Check the API key for this provider",
		},
		{
			name:     "context overflow reroutes to /compact",
			err:      statusErr(400, `{"message":"This model's maximum context length is 8192 tokens","code":"context_length_exceeded"}`),
			headline: "Context window exceeded (400)",
			detail:   "This model's maximum context length is 8192 tokens",
			hint:     "Try /compact to shrink the conversation",
		},
		{
			name:     "anthropic prompt-too-long phrasing",
			err:      statusErr(400, `{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}`),
			headline: "Context window exceeded (400)",
			detail:   "prompt is too long: 210000 tokens > 200000 maximum",
			hint:     "Try /compact to shrink the conversation",
		},
		{
			name:     "non-JSON body falls back verbatim",
			err:      statusErr(502, "upstream connect error"),
			headline: "Provider server error (502)",
			detail:   "upstream connect error",
		},
		{
			name:     "bare string error value",
			err:      statusErr(400, `"invalid request"`),
			headline: "Request rejected (400 Status Text)",
			detail:   "invalid request",
		},
		{
			name:     "nested envelope from a proxy",
			err:      statusErr(404, `{"error":{"message":"model x does not exist"}}`),
			headline: "Not found (404)",
			detail:   "model x does not exist",
			hint:     "Check the model name (/model) and base URL",
		},
		{
			name:     "permanent wrapper is seen through",
			err:      &provider.PermanentError{Err: statusErr(429, `{"message":"quota"}`)},
			headline: "Rate limited (429)",
			detail:   "quota",
		},
		{
			name:     "no SSE events",
			err:      llm.ErrNoEvents,
			headline: "Provider did not stream",
			detail:   llm.ErrNoEvents.Error(),
		},
		{
			name:     "plain error",
			err:      errors.New("tool rounds exceeded"),
			headline: "Request failed",
			detail:   "tool rounds exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := describeError(tc.err)
			if r.Headline != tc.headline {
				t.Errorf("headline = %q, want %q", r.Headline, tc.headline)
			}
			got := strings.Join(r.Detail, "\n")
			if got != tc.detail {
				t.Errorf("detail = %q, want %q", got, tc.detail)
			}
			if r.Hint != tc.hint {
				t.Errorf("hint = %q, want %q", r.Hint, tc.hint)
			}
		})
	}
}

// The report's lines() appends the hint after the detail rows.
func TestErrorReportLines(t *testing.T) {
	r := errorReport{Detail: []string{"a", "b"}, Hint: "h"}
	if got := strings.Join(r.lines(), "|"); got != "a|b|h" {
		t.Fatalf("lines = %q", got)
	}
	r.Hint = ""
	if got := strings.Join(r.lines(), "|"); got != "a|b" {
		t.Fatalf("lines without hint = %q", got)
	}
}

// errorBlock renders headline + tool-result-idiom detail rows in ONE block
// (one separator), and groups with adjacent error output like error().
func TestTranscriptErrorBlock(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("hi")
	tr.errorBlock("Rate limited (429)", "Rate limit reached", "second row")
	tr.errorBlock("Provider server error (500)") // consecutive: same block, no separator

	want := []string{
		"user:hi",
		"print:", // one separator opens the error block
		"print:" + strings.Join([]string{
			ErrorStyle.Sprint("✗ Rate limited (429)"),
			DimStyle.Sprint("  ⎿ Rate limit reached"),
			DimStyle.Sprint("    second row"),
		}, "|"),
		"print:" + ErrorStyle.Sprint("✗ Provider server error (500)"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Overlong detail rows pre-wrap under the hanging indent (recSurface reports
// width 80 → wrap at 75), so region-level wrapping never restarts a
// continuation row at column zero.
func TestTranscriptErrorBlockHangingWrap(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)
	tr.user("hi")
	long := strings.Repeat("x", 100)
	tr.errorBlock("Rate limited (429)", long)

	want := []string{
		"user:hi",
		"print:",
		"print:" + strings.Join([]string{
			ErrorStyle.Sprint("✗ Rate limited (429)"),
			DimStyle.Sprint("  ⎿ " + long[:75]),
			DimStyle.Sprint("    " + long[75:]),
		}, "|"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Detail entries carrying embedded newlines are split and indented per row.
func TestTranscriptErrorBlockMultiline(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)
	tr.user("hi")
	tr.errorBlock("Request failed", "line one\nline two\n\n")

	want := []string{
		"user:hi",
		"print:",
		"print:" + strings.Join([]string{
			ErrorStyle.Sprint("✗ Request failed"),
			DimStyle.Sprint("  ⎿ line one"),
			DimStyle.Sprint("    line two"),
		}, "|"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}
