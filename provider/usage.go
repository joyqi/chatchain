package provider

import "chatchain/internal/llm"

// Per-dialect usage conversion. The wire shapes disagree on what "input"
// covers and on whether a total exists at all, so each converter states its
// dialect's contract here rather than leaving Usage.ContextTokens to guess.

// chatUsage converts OpenAI chat-completions usage. prompt_tokens ALREADY
// includes the cache hits, so CacheRead is recorded for display only and the
// authoritative total comes from the wire.
func chatUsage(u *llm.ChatUsage) Usage {
	out := Usage{
		Input:  u.PromptTokens,
		Output: u.CompletionTokens,
		Total:  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output // compat servers often omit it
	}
	return out
}

// respUsage converts OpenAI Responses usage — same cache contract as
// chat-completions (input_tokens includes the cached share), and output_tokens
// already covers reasoning tokens.
func respUsage(u *llm.RespUsage) Usage {
	out := Usage{
		Input:  u.InputTokens,
		Output: u.OutputTokens,
		Total:  u.TotalTokens,
	}
	if u.InputTokensDetails != nil {
		out.CacheRead = u.InputTokensDetails.CachedTokens
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output
	}
	return out
}

// anthropicUsage converts Anthropic usage. Anthropic reports NO total, and its
// input_tokens excludes both cache figures — they are additional context, not
// a subset — so Total is deliberately left at zero for ContextTokens to sum
// the parts.
func anthropicUsage(u llm.AnthropicUsage) Usage {
	return Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

// googleUsage converts Gemini usage. candidatesTokenCount EXCLUDES thinking
// tokens while totalTokenCount includes them, which is precisely why the total
// is authoritative for context accounting; Output keeps the thinking share so
// the session's cumulative figure matches what was billed.
func googleUsage(u *llm.GUsageMetadata) Usage {
	out := Usage{
		Input:     u.PromptTokenCount,
		Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Total:     u.TotalTokenCount,
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output
	}
	return out
}
