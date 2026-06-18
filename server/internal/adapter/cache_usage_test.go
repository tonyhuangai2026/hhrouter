package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// cache_usage_test.go verifies that all three adapters surface prompt-cache
// token counts (CacheReadTokens / CacheWriteTokens) on adapter.Usage, from both
// the non-stream ParseResponse and the streaming ParseStreamChunk, and that a
// response without cache fields leaves them at 0 (back-compat).

func TestOpenAICacheTokens(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})

	// Non-stream: prompt_tokens_details.cached_tokens → CacheReadTokens.
	body := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,
		"prompt_tokens_details":{"cached_tokens":64}}}`
	_, usage, err := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.CacheReadTokens != 64 || usage.CacheWriteTokens != 0 {
		t.Errorf("non-stream cache = read %d / write %d, want 64 / 0", usage.CacheReadTokens, usage.CacheWriteTokens)
	}

	// Stream final-usage chunk.
	c, ok, err := a.ParseStreamChunk("", []byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64}}}`))
	if err != nil || !ok || c.Usage == nil {
		t.Fatalf("stream usage chunk: ok=%v err=%v chunk=%+v", ok, err, c)
	}
	if c.Usage.CacheReadTokens != 64 {
		t.Errorf("stream cache read = %d, want 64", c.Usage.CacheReadTokens)
	}

	// No details → 0 (back-compat).
	_, u2, _ := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
		`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))})
	if u2.CacheReadTokens != 0 || u2.CacheWriteTokens != 0 {
		t.Errorf("missing details cache = %d/%d, want 0/0", u2.CacheReadTokens, u2.CacheWriteTokens)
	}
}

func TestAnthropicCacheTokens(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})

	// Non-stream: cache_read_input_tokens / cache_creation_input_tokens.
	body := `{"type":"message","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":80,"cache_creation_input_tokens":40}}`
	_, usage, err := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.CacheReadTokens != 80 || usage.CacheWriteTokens != 40 {
		t.Errorf("non-stream cache = read %d / write %d, want 80 / 40", usage.CacheReadTokens, usage.CacheWriteTokens)
	}

	// Stream: cache tokens arrive on message_start alongside input_tokens.
	c, ok, err := a.ParseStreamChunk("", []byte(
		`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":80,"cache_creation_input_tokens":40}}}`))
	if err != nil || !ok || c.Usage == nil {
		t.Fatalf("message_start: ok=%v err=%v chunk=%+v", ok, err, c)
	}
	if c.Usage.CacheReadTokens != 80 || c.Usage.CacheWriteTokens != 40 {
		t.Errorf("stream cache = read %d / write %d, want 80 / 40", c.Usage.CacheReadTokens, c.Usage.CacheWriteTokens)
	}

	// No cache fields → 0.
	_, u2, _ := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
		`{"type":"message","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))})
	if u2.CacheReadTokens != 0 || u2.CacheWriteTokens != 0 {
		t.Errorf("missing cache = %d/%d, want 0/0", u2.CacheReadTokens, u2.CacheWriteTokens)
	}
}

func TestBedrockCacheTokens(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})

	// Non-stream Converse usage.
	body := `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn",
		"usage":{"inputTokens":50,"outputTokens":10,"totalTokens":60,"cacheReadInputTokens":30,"cacheWriteInputTokens":20}}`
	_, usage, err := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.CacheReadTokens != 30 || usage.CacheWriteTokens != 20 {
		t.Errorf("non-stream cache = read %d / write %d, want 30 / 20", usage.CacheReadTokens, usage.CacheWriteTokens)
	}

	// Stream metadata event usage.
	c, ok, err := a.ParseStreamChunk("metadata", []byte(
		`{"usage":{"inputTokens":50,"outputTokens":10,"totalTokens":60,"cacheReadInputTokens":30,"cacheWriteInputTokens":20}}`))
	if err != nil || !ok || c.Usage == nil {
		t.Fatalf("metadata: ok=%v err=%v chunk=%+v", ok, err, c)
	}
	if c.Usage.CacheReadTokens != 30 || c.Usage.CacheWriteTokens != 20 {
		t.Errorf("stream cache = read %d / write %d, want 30 / 20", c.Usage.CacheReadTokens, c.Usage.CacheWriteTokens)
	}

	// No cache fields → 0.
	_, u2, _ := a.ParseResponse(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
		`{"output":{"message":{"content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}`))})
	if u2.CacheReadTokens != 0 || u2.CacheWriteTokens != 0 {
		t.Errorf("missing cache = %d/%d, want 0/0", u2.CacheReadTokens, u2.CacheWriteTokens)
	}
}
