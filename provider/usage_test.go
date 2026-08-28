package provider

import (
	"testing"

	"github.com/joyqi/iota/internal/llm"
)

// ContextTokens must read each dialect's contract, not one hardcoded formula:
// the provider's own total wins where it exists (it covers classes we don't
// itemize), and the parts are summed only where no total is reported.
func TestUsageContextTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    Usage
		want int
	}{
		{
			// OpenAI: cached tokens are INSIDE Input; adding them would
			// double-count, so the wire total is what counts.
			name: "total wins over the itemized parts",
			u:    Usage{Input: 1000, Output: 200, CacheRead: 800, Total: 1200},
			want: 1200,
		},
		{
			// Anthropic: no total, and cache reads/writes sit BESIDE input.
			name: "no total sums every part",
			u:    Usage{Input: 100, Output: 200, CacheRead: 3000, CacheWrite: 500},
			want: 3800,
		},
		{
			name: "plain in/out",
			u:    Usage{Input: 10, Output: 5},
			want: 15,
		},
		{name: "zero", u: Usage{}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.ContextTokens(); got != tc.want {
				t.Fatalf("ContextTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Each converter encodes its dialect's cache/total contract. Getting these
// wrong silently mis-sizes the context window, so they are pinned here.
func TestDialectUsageConversion(t *testing.T) {
	t.Run("chatcomp keeps cached inside input", func(t *testing.T) {
		u := chatUsage(&llm.ChatUsage{
			PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
			PromptTokensDetails: &struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: 768},
		})
		if u.Input != 1000 || u.CacheRead != 768 {
			t.Fatalf("usage = %+v", u)
		}
		if got := u.ContextTokens(); got != 1200 {
			t.Fatalf("ContextTokens = %d, want 1200 (cached must not be added on top)", got)
		}
	})

	t.Run("chatcomp without a total falls back to the sum", func(t *testing.T) {
		// Compat servers (local gateways) often omit total_tokens.
		u := chatUsage(&llm.ChatUsage{PromptTokens: 30, CompletionTokens: 12})
		if got := u.ContextTokens(); got != 42 {
			t.Fatalf("ContextTokens = %d, want 42", got)
		}
	})

	t.Run("anthropic adds cache beside input", func(t *testing.T) {
		u := anthropicUsage(llm.AnthropicUsage{
			InputTokens: 120, OutputTokens: 80,
			CacheReadInputTokens: 9000, CacheCreationInputTokens: 1000,
		})
		if u.Total != 0 {
			t.Fatalf("anthropic must not invent a total: %+v", u)
		}
		if got := u.ContextTokens(); got != 10200 {
			t.Fatalf("ContextTokens = %d, want 10200 (cache is additional context)", got)
		}
	})

	t.Run("google counts thinking tokens", func(t *testing.T) {
		// candidatesTokenCount EXCLUDES thoughts; totalTokenCount includes them.
		u := googleUsage(&llm.GUsageMetadata{
			PromptTokenCount: 5000, CandidatesTokenCount: 300,
			ThoughtsTokenCount: 2000, TotalTokenCount: 7300,
		})
		if u.Output != 2300 {
			t.Fatalf("Output = %d, want 2300 (candidates + thoughts)", u.Output)
		}
		if got := u.ContextTokens(); got != 7300 {
			t.Fatalf("ContextTokens = %d, want the reported total 7300", got)
		}
	})

	t.Run("responses keeps cached inside input", func(t *testing.T) {
		u := respUsage(&llm.RespUsage{
			InputTokens: 900, OutputTokens: 100, TotalTokens: 1000,
			InputTokensDetails: &struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: 640},
		})
		if got := u.ContextTokens(); got != 1000 {
			t.Fatalf("ContextTokens = %d, want 1000", got)
		}
	})
}

// The cache-hit ratio has to be read against the RIGHT denominator, which is
// dialect-specific: OpenAI and Gemini file cached tokens inside Input, while
// Anthropic files them beside it. Using Input blindly would report the same
// cache activity as two very different percentages.
func TestUsagePromptTokensAndHitRate(t *testing.T) {
	t.Run("cache inside input", func(t *testing.T) {
		// 13056 of a 17000-token prompt came from cache.
		u := Usage{Input: 17000, Output: 500, CacheRead: 13056, Total: 17500}
		if got := u.PromptTokens(); got != 17000 {
			t.Fatalf("PromptTokens = %d, want 17000 (cache already inside)", got)
		}
		if got := u.CacheHitRate(); got < 76.7 || got > 76.9 {
			t.Fatalf("hit rate = %.2f, want ~76.8", got)
		}
	})

	t.Run("cache beside input", func(t *testing.T) {
		// Anthropic: 200 fresh tokens on top of a 9800-token cached prefix.
		u := Usage{Input: 200, Output: 300, CacheRead: 9800}
		if got := u.PromptTokens(); got != 10000 {
			t.Fatalf("PromptTokens = %d, want 10000 (cache is additional)", got)
		}
		if got := u.CacheHitRate(); got != 98 {
			t.Fatalf("hit rate = %.2f, want 98", got)
		}
	})

	t.Run("no cache reports nothing", func(t *testing.T) {
		u := Usage{Input: 1000, Output: 100, Total: 1100}
		if u.Cached() {
			t.Fatal("Cached() true without any cache activity")
		}
		if got := u.CacheHitRate(); got != 0 {
			t.Fatalf("hit rate = %.2f, want 0", got)
		}
	})

	t.Run("empty usage", func(t *testing.T) {
		var u Usage
		if u.Cached() || u.CacheHitRate() != 0 || u.PromptTokens() != 0 {
			t.Fatalf("zero usage misreports: %+v", u)
		}
	})
}
