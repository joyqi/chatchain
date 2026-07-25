package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"chatchain/internal/llm"
)

// errorReport is a turn error shaped for display: a short classification
// headline, the provider's human-readable message (or the raw error text as a
// fallback — no information is ever dropped), and an optional actionable hint.
type errorReport struct {
	Headline string
	Detail   []string
	Hint     string
}

// lines flattens the report into the errorBlock detail rows (hint last).
func (r errorReport) lines() []string {
	if r.Hint == "" {
		return r.Detail
	}
	return append(append([]string{}, r.Detail...), r.Hint)
}

// describeError classifies a turn-fatal error for the structured error block.
// Wire errors (*llm.StatusError) get a status-class headline plus their
// envelope's message field; everything else keeps its error text as detail
// under a generic headline.
func describeError(err error) errorReport {
	var se *llm.StatusError
	if errors.As(err, &se) {
		return describeStatus(se)
	}
	if errors.Is(err, llm.ErrNoEvents) {
		return errorReport{Headline: "Provider did not stream", Detail: []string{err.Error()}}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errorReport{Headline: "Network error", Detail: []string{err.Error()}}
	}
	return errorReport{Headline: "Request failed", Detail: []string{err.Error()}}
}

func describeStatus(se *llm.StatusError) errorReport {
	detail, ok := envelopeMessage(se.Body)
	if !ok {
		detail = se.Body
	}
	r := errorReport{Detail: nonEmptyLines(detail)}
	switch {
	case se.Status == 401 || se.Status == 403:
		r.Headline = fmt.Sprintf("Authentication failed (%d)", se.Status)
		r.Hint = "Check the API key for this provider"
	case se.Status == 402:
		r.Headline = "Billing issue (402)"
	case se.Status == 404:
		r.Headline = "Not found (404)"
		r.Hint = "Check the model name (/model) and base URL"
	case se.Status == 408:
		r.Headline = "Request timed out (408)"
	case se.Status == 413:
		r.Headline = "Request too large (413)"
	case se.Status == 429:
		r.Headline = "Rate limited (429)"
	case se.Status >= 500:
		r.Headline = fmt.Sprintf("Provider server error (%d)", se.Status)
	default:
		r.Headline = fmt.Sprintf("Request rejected (%d %s)", se.Status, se.StatusText)
	}
	if contextOverflow(detail) {
		r.Headline = fmt.Sprintf("Context window exceeded (%d)", se.Status)
		r.Hint = "Try /compact to shrink the conversation"
	}
	return r
}

// envelopeMessage extracts the human-readable message from a provider error
// envelope. internal/llm already unwrapped the top-level "error" value, so the
// dialect shapes are: openai {"message":…,"type":…}, google {"code":…,
// "message":…,"status":…}, anthropic {"type":…,"message":…} — all carry
// "message". A bare JSON string (an "error" value that wasn't an object) and
// one extra nesting level (proxies passing the whole envelope through) are
// also accepted.
func envelopeMessage(body string) (string, bool) {
	var s string
	if json.Unmarshal([]byte(body), &s) == nil {
		return s, s != ""
	}
	var env struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return "", false
	}
	if env.Message != "" {
		return env.Message, true
	}
	return env.Error.Message, env.Error.Message != ""
}

// contextOverflow reports whether a provider message describes the request
// exceeding the model's context window — the one 4xx with a specific in-chat
// remedy (/compact).
func contextOverflow(msg string) bool {
	lower := strings.ToLower(msg)
	for _, pat := range []string{
		"context length", "context_length", "context window",
		"maximum context", "too many tokens", "input token count",
		"prompt is too long",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// nonEmptyLines splits s into trimmed-right rows, dropping blank ones.
func nonEmptyLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimRight(ln, "\r"); strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}
