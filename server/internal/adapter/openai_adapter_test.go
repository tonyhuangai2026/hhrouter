package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// stubDecryptor returns a fixed plaintext key, simulating ChannelService.Decrypt.
type stubDecryptor struct{ key string }

func (s stubDecryptor) Decrypt(*model.Channel) (string, error) { return s.key, nil }

func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return b
}

func TestOpenAIAdapter_BuildRequest(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "sk-test-123"})
	ch := &model.Channel{Type: model.ChannelOpenAI, BaseURL: "https://api.example.com/"}

	uni := UnifiedRequest{
		Model:     "gpt-4o",
		System:    "be brief",
		Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}}},
		Stream:    true,
		MaxTokens: intPtr(64),
	}

	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	// URL must be {base_url}/v1/chat/completions with trailing slash trimmed.
	if got, want := req.URL.String(), "https://api.example.com/v1/chat/completions"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	// Auth header.
	if got, want := req.Header.Get("Authorization"), "Bearer sk-test-123"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body openAIChatRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", body.Model)
	}
	if !body.Stream {
		t.Error("stream should be true")
	}
	if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage should be set when streaming")
	}
	if body.MaxTokens == nil || *body.MaxTokens != 64 {
		t.Errorf("max_tokens = %v, want 64", body.MaxTokens)
	}
	// System must be the leading message.
	if len(body.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user)", len(body.Messages))
	}
	if body.Messages[0].Role != RoleSystem || body.Messages[0].Content != "be brief" {
		t.Errorf("first message = %+v, want system/be brief", body.Messages[0])
	}
	if body.Messages[1].Role != RoleUser || body.Messages[1].Content != "hello" {
		t.Errorf("second message = %+v, want user/hello", body.Messages[1])
	}
}

func TestOpenAIAdapter_BuildRequest_NoStreamNoSystem(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelOpenAI, BaseURL: "https://api.example.com"}
	uni := UnifiedRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}}}

	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body openAIChatRequest
	_ = json.Unmarshal(readBody(t, req), &body)
	if body.Stream {
		t.Error("stream should be false")
	}
	if body.StreamOptions != nil {
		t.Error("stream_options should be omitted when not streaming")
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1 (no system)", len(body.Messages))
	}
}

func TestOpenAIAdapter_BuildRequest_MissingBaseURL(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelOpenAI}
	if _, err := a.BuildRequest(context.Background(), UnifiedRequest{Model: "m"}, ch); err == nil {
		t.Fatal("expected error for missing base_url")
	}
}

func TestOpenAIAdapter_ParseResponse(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	respBody := `{
		"model":"gpt-4o",
		"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"length"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(respBody))}

	out, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Text() != "hi there" {
		t.Errorf("text = %q", out.Text())
	}
	if out.StopReason != StopMaxTokens {
		t.Errorf("stop = %q, want max_tokens", out.StopReason)
	}
	if !usage.HasUpstream || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestOpenAIAdapter_ParseResponse_UsageFallback(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	// No usage field → estimate from completion text ("12345678" = 8 chars → 2 tokens).
	respBody := `{"model":"m","choices":[{"message":{"content":"12345678"},"finish_reason":"stop"}]}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(respBody))}

	_, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.HasUpstream {
		t.Error("usage should be estimated, not upstream")
	}
	if usage.CompletionTokens != 2 {
		t.Errorf("estimated completion = %d, want 2", usage.CompletionTokens)
	}
}

func TestOpenAIAdapter_ParseResponse_HTTPError(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	resp := &http.Response{
		StatusCode: 503,
		Status:     "503 Service Unavailable",
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"overloaded"}}`)),
	}
	_, _, err := a.ParseResponse(resp)
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.StatusCode != 503 || ue.Message != "overloaded" {
		t.Errorf("UpstreamError = %+v", ue)
	}
	if !ue.Retryable() {
		t.Error("503 should be retryable")
	}
}

func TestOpenAIAdapter_ParseStreamChunk(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})

	// content delta (OpenAI is plain SSE → eventType is always "")
	c, ok, err := a.ParseStreamChunk("", []byte(`{"model":"m","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`))
	if err != nil || !ok {
		t.Fatalf("delta chunk: ok=%v err=%v", ok, err)
	}
	if c.Delta != "Hel" {
		t.Errorf("delta = %q", c.Delta)
	}

	// finish chunk
	c, ok, _ = a.ParseStreamChunk("", []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	if !ok || c.StopReason != StopEndTurn {
		t.Errorf("finish chunk: ok=%v stop=%q", ok, c.StopReason)
	}

	// usage tail chunk
	c, ok, _ = a.ParseStreamChunk("", []byte(`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`))
	if !ok || c.Usage == nil || c.Usage.TotalTokens != 10 {
		t.Errorf("usage chunk: ok=%v usage=%+v", ok, c.Usage)
	}

	// [DONE] sentinel
	c, ok, _ = a.ParseStreamChunk("", []byte("[DONE]"))
	if !c.Done {
		t.Error("[DONE] should set Done")
	}

	// OpenAI ignores any (spurious) eventType and still parses the data body.
	c, ok, _ = a.ParseStreamChunk("contentBlockDelta", []byte(`{"choices":[{"delta":{"content":"X"},"finish_reason":null}]}`))
	if !ok || c.Delta != "X" {
		t.Errorf("openai must ignore eventType: ok=%v delta=%q", ok, c.Delta)
	}

	// empty keepalive
	_, ok, _ = a.ParseStreamChunk("", []byte("   "))
	if ok {
		t.Error("blank chunk should not be meaningful")
	}
}
