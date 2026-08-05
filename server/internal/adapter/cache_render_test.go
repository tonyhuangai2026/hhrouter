package adapter

import "testing"

// cache_render_test.go covers the CLIENT-FACING rendering of prompt-cache tokens
// (the counterpart to cache_usage_test.go, which covers parsing them IN). It
// asserts that a cache-bearing unified Usage renders into each output format's
// native cache fields on both the non-streaming Build*Response and the streaming
// helpers, and — critically for backward compatibility — that a zero-cache Usage
// leaves the usage shape byte-for-byte unchanged (NO cache keys).

// cacheUsage is the canonical cache-bearing Usage used across these tests:
// 100 prompt / 20 completion / 120 total, 30 cache-read + 20 cache-write.
func cacheUsage() Usage {
	return Usage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
		CacheReadTokens:  30,
		CacheWriteTokens: 20,
		HasUpstream:      true,
	}
}

// zeroCacheUsage is the same token totals with NO cache activity — the
// back-compat baseline (must render zero cache keys).
func zeroCacheUsage() Usage {
	return Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, HasUpstream: true}
}

// ---- non-streaming Build*Response --------------------------------------------

func TestBuildOpenAIResponse_CacheUsage(t *testing.T) {
	r := UnifiedResponse{Model: "gpt-4o", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: cacheUsage()}
	usage := BuildOpenAIResponse(r)["usage"].(map[string]any)

	// Canonical fields untouched.
	if usage["prompt_tokens"] != 100 || usage["completion_tokens"] != 20 || usage["total_tokens"] != 120 {
		t.Fatalf("base usage = %+v, want 100/20/120", usage)
	}
	// Both cache buckets are reported under prompt_tokens_details: cached_tokens is
	// OpenAI's own field for hits, and cache_creation_tokens carries the write count
	// that OpenAI's schema has no field for (omitting it made a Bedrock channel look
	// like it never wrote to cache when driven from an OpenAI client).
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details missing/not a map: %+v", usage)
	}
	if details["cached_tokens"] != 30 {
		t.Errorf("cached_tokens = %v, want 30", details["cached_tokens"])
	}
	if details["cache_creation_tokens"] != 20 {
		t.Errorf("cache_creation_tokens = %v, want 20", details["cache_creation_tokens"])
	}
	// prompt_tokens is deliberately NOT inflated by the cache buckets: it is an
	// existing field clients meter on, and Usage.PromptTokens holds the upstream's
	// cache-excluding count.
	if usage["prompt_tokens"] != 100 {
		t.Errorf("prompt_tokens = %v, want the upstream 100 (unchanged)", usage["prompt_tokens"])
	}
}

// A write-only response (first call of a cache pair) must still report the write.
// This is the exact case the customer saw as "no write cache" on an OpenAI client.
func TestBuildOpenAIResponse_WriteOnlyCacheUsage(t *testing.T) {
	u := Usage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11, CacheWriteTokens: 53974, HasUpstream: true}
	r := UnifiedResponse{Model: "gpt-4o", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: u}
	usage := BuildOpenAIResponse(r)["usage"].(map[string]any)
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("write-only usage must carry prompt_tokens_details: %+v", usage)
	}
	if details["cache_creation_tokens"] != 53974 {
		t.Errorf("cache_creation_tokens = %v, want 53974", details["cache_creation_tokens"])
	}
	if _, present := details["cached_tokens"]; present {
		t.Errorf("no cache hit occurred, so cached_tokens must be absent: %+v", details)
	}
}

func TestBuildOpenAIResponse_ZeroCacheNoKeys(t *testing.T) {
	r := UnifiedResponse{Model: "gpt-4o", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: zeroCacheUsage()}
	usage := BuildOpenAIResponse(r)["usage"].(map[string]any)

	if _, present := usage["prompt_tokens_details"]; present {
		t.Errorf("zero-cache usage must NOT carry prompt_tokens_details: %+v", usage)
	}
	// Shape unchanged: exactly the three canonical keys.
	if len(usage) != 3 {
		t.Errorf("zero-cache usage should have exactly 3 keys, got %d: %+v", len(usage), usage)
	}
}

func TestBuildAnthropicResponse_CacheUsage(t *testing.T) {
	r := UnifiedResponse{Model: "claude-x", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: cacheUsage()}
	usage := BuildAnthropicResponse(r)["usage"].(map[string]any)

	if usage["input_tokens"] != 100 || usage["output_tokens"] != 20 {
		t.Fatalf("base usage = %+v, want input 100 / output 20", usage)
	}
	if usage["cache_read_input_tokens"] != 30 {
		t.Errorf("cache_read_input_tokens = %v, want 30", usage["cache_read_input_tokens"])
	}
	if usage["cache_creation_input_tokens"] != 20 {
		t.Errorf("cache_creation_input_tokens = %v, want 20", usage["cache_creation_input_tokens"])
	}
}

func TestBuildAnthropicResponse_ZeroCacheNoKeys(t *testing.T) {
	r := UnifiedResponse{Model: "claude-x", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: zeroCacheUsage()}
	usage := BuildAnthropicResponse(r)["usage"].(map[string]any)

	for _, k := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if _, present := usage[k]; present {
			t.Errorf("zero-cache anthropic usage must NOT carry %q: %+v", k, usage)
		}
	}
	// Shape unchanged: exactly input_tokens + output_tokens.
	if len(usage) != 2 {
		t.Errorf("zero-cache anthropic usage should have exactly 2 keys, got %d: %+v", len(usage), usage)
	}
}

func TestBuildBedrockResponse_CacheUsage(t *testing.T) {
	r := UnifiedResponse{Model: "claude-x", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: cacheUsage()}
	usage := BuildBedrockResponse(r)["usage"].(map[string]any)

	if usage["inputTokens"] != 100 || usage["outputTokens"] != 20 || usage["totalTokens"] != 120 {
		t.Fatalf("base usage = %+v, want 100/20/120", usage)
	}
	if usage["cacheReadInputTokens"] != 30 {
		t.Errorf("cacheReadInputTokens = %v, want 30", usage["cacheReadInputTokens"])
	}
	if usage["cacheWriteInputTokens"] != 20 {
		t.Errorf("cacheWriteInputTokens = %v, want 20", usage["cacheWriteInputTokens"])
	}
}

func TestBuildBedrockResponse_ZeroCacheNoKeys(t *testing.T) {
	r := UnifiedResponse{Model: "claude-x", Content: []ContentBlock{TextBlock("hi")}, StopReason: StopEndTurn, Usage: zeroCacheUsage()}
	usage := BuildBedrockResponse(r)["usage"].(map[string]any)

	for _, k := range []string{"cacheReadInputTokens", "cacheWriteInputTokens"} {
		if _, present := usage[k]; present {
			t.Errorf("zero-cache bedrock usage must NOT carry %q: %+v", k, usage)
		}
	}
	// Shape unchanged: exactly inputTokens + outputTokens + totalTokens.
	if len(usage) != 3 {
		t.Errorf("zero-cache bedrock usage should have exactly 3 keys, got %d: %+v", len(usage), usage)
	}
}

// ---- streaming helpers -------------------------------------------------------

func TestBuildOpenAIStreamChunk_CacheUsage(t *testing.T) {
	u := cacheUsage()
	out := BuildOpenAIStreamChunk("m", StreamChunk{Usage: &u})
	usage := out["usage"].(map[string]any)

	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("stream chunk prompt_tokens_details missing: %+v", usage)
	}
	if details["cached_tokens"] != 30 {
		t.Errorf("stream cached_tokens = %v, want 30", details["cached_tokens"])
	}
	if details["cache_creation_tokens"] != 20 {
		t.Errorf("stream cache_creation_tokens = %v, want 20", details["cache_creation_tokens"])
	}

	// Zero cache → no details key on the stream chunk usage.
	z := zeroCacheUsage()
	zusage := BuildOpenAIStreamChunk("m", StreamChunk{Usage: &z})["usage"].(map[string]any)
	if _, present := zusage["prompt_tokens_details"]; present {
		t.Errorf("zero-cache stream chunk must NOT carry prompt_tokens_details: %+v", zusage)
	}
}

func TestBedrockMetadataPayload_CacheUsage(t *testing.T) {
	usage := BedrockMetadataPayload(cacheUsage())["usage"].(map[string]any)

	if usage["inputTokens"] != 100 || usage["outputTokens"] != 20 || usage["totalTokens"] != 120 {
		t.Fatalf("metadata base usage = %+v, want 100/20/120", usage)
	}
	if usage["cacheReadInputTokens"] != 30 || usage["cacheWriteInputTokens"] != 20 {
		t.Errorf("metadata cache = read %v / write %v, want 30 / 20", usage["cacheReadInputTokens"], usage["cacheWriteInputTokens"])
	}

	// Zero cache → no cache keys.
	zusage := BedrockMetadataPayload(zeroCacheUsage())["usage"].(map[string]any)
	if len(zusage) != 3 {
		t.Errorf("zero-cache metadata usage should have exactly 3 keys, got %d: %+v", len(zusage), zusage)
	}
}

func TestAnthropicStreamDeltaUsage(t *testing.T) {
	// Full: output always; input (>0) and both cache buckets (>0) present.
	full := AnthropicStreamDeltaUsage(cacheUsage())
	if full["output_tokens"] != 20 {
		t.Errorf("output_tokens = %v, want 20", full["output_tokens"])
	}
	if full["input_tokens"] != 100 {
		t.Errorf("input_tokens = %v, want 100", full["input_tokens"])
	}
	if full["cache_read_input_tokens"] != 30 || full["cache_creation_input_tokens"] != 20 {
		t.Errorf("delta cache = read %v / write %v, want 30 / 20", full["cache_read_input_tokens"], full["cache_creation_input_tokens"])
	}

	// Zero cache + zero prompt → ONLY output_tokens (no input, no cache keys).
	only := AnthropicStreamDeltaUsage(Usage{CompletionTokens: 9})
	if only["output_tokens"] != 9 {
		t.Errorf("output_tokens = %v, want 9", only["output_tokens"])
	}
	if len(only) != 1 {
		t.Errorf("zero/zero-prompt delta usage should have exactly 1 key (output_tokens), got %d: %+v", len(only), only)
	}

	// Prompt but no cache → output + input, still no cache keys.
	noCache := AnthropicStreamDeltaUsage(Usage{PromptTokens: 50, CompletionTokens: 7})
	if noCache["input_tokens"] != 50 || noCache["output_tokens"] != 7 {
		t.Errorf("no-cache delta = %+v, want input 50 / output 7", noCache)
	}
	if len(noCache) != 2 {
		t.Errorf("no-cache delta usage should have exactly 2 keys (input+output), got %d: %+v", len(noCache), noCache)
	}
}
