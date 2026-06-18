package adapter

// usage.go centralizes token-usage parsing (Tech Design §6: "response usage
// prefers the upstream's real tokens, falling back to a char-based estimate when
// missing"). Each upstream reports usage under a different shape; the helpers
// here normalize them to Usage and provide the estimate fallback.

// Usage is the normalized token accounting attached to responses and terminal
// stream chunks. PromptTokens / CompletionTokens / TotalTokens are the canonical
// fields used by the relay for quota Consume and request logging.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// CacheReadTokens / CacheWriteTokens are prompt-cache token counts, priced on
	// their own tiers (see pricing). CacheReadTokens = tokens served from a cache
	// hit (OpenAI prompt_tokens_details.cached_tokens, Anthropic
	// cache_read_input_tokens, Bedrock cacheReadInputTokens). CacheWriteTokens =
	// tokens written into the cache this turn (Anthropic cache_creation_input_tokens,
	// Bedrock cacheWriteInputTokens; OpenAI has no separate write count → 0). They
	// default to 0 for upstreams/versions that don't report caching.
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	// HasUpstream reports whether these numbers came from the upstream (true) or
	// from a char-based estimate (false). The relay can flag estimated usage.
	HasUpstream bool `json:"-"`
	// Estimated is the inverse of HasUpstream, exposed for logging/metrics.
	Estimated bool `json:"-"`
}

// normalize fills TotalTokens from the parts when the upstream omitted it (or
// reported an inconsistent total) and keeps prompt/completion authoritative.
func (u Usage) normalize() Usage {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

// usageFromOpenAI converts an OpenAI usage object to Usage. A nil pointer (no
// usage reported) yields a zero Usage with HasUpstream=false.
func usageFromOpenAI(u *openAIUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		HasUpstream:      true,
	}
	// OpenAI reports cache hits under prompt_tokens_details.cached_tokens; there is
	// no separate cache-write count. Absent details → 0.
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return out.normalize()
}

// usageFromBedrock converts a Bedrock TokenUsage object to Usage. inputTokens →
// prompt, outputTokens → completion, cacheRead/WriteInputTokens → cache buckets.
func usageFromBedrock(u *bedrockUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheWriteInputTokens,
		HasUpstream:      true,
	}
	return out.normalize()
}

// estimateTokens approximates the token count of a piece of text. The MVP uses
// the common ~4-chars-per-token heuristic (Tech Design §6), rounding up so any
// non-empty text counts as at least one token.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// ceil(len/4).
	return (len(text) + 3) / 4
}

// estimateUsageFromText builds an estimated Usage from the prompt and completion
// text when the upstream did not report usage. The result is marked Estimated
// (HasUpstream=false) so callers can distinguish it from authoritative counts.
func estimateUsageFromText(promptText, completionText string) Usage {
	p := estimateTokens(promptText)
	c := estimateTokens(completionText)
	return Usage{
		PromptTokens:     p,
		CompletionTokens: c,
		TotalTokens:      p + c,
		HasUpstream:      false,
		Estimated:        true,
	}
}

// EstimatePromptTokens is an exported helper for the relay's pre-flight quota
// check (Tech Design §4/§6): it estimates input tokens from the unified request's
// system prompt and message text before any upstream call.
func EstimatePromptTokens(uni UnifiedRequest) int {
	total := estimateTokens(uni.System)
	for _, m := range uni.Messages {
		total += estimateTokens(m.Text())
	}
	return total
}
