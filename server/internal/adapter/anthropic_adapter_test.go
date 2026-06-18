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

func TestAnthropicAdapter_Name(t *testing.T) {
	if got := NewAnthropicAdapter(stubDecryptor{}).Name(); got != "anthropic" {
		t.Fatalf("Name() = %q, want anthropic", got)
	}
	// For() must dispatch the new channel type to this adapter.
	ad, ok := For(&model.Channel{Type: model.ChannelAnthropic}, stubDecryptor{})
	if !ok || ad.Name() != "anthropic" {
		t.Fatalf("For(anthropic) = %v, %v", ad, ok)
	}
}

func TestAnthropicAdapter_BuildRequest(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{key: "sk-ant-test"})
	ch := &model.Channel{Type: model.ChannelAnthropic, BaseURL: "https://api.anthropic.com/"}
	uni := UnifiedRequest{
		Model:    "claude-opus-4-8",
		System:   "be brief",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
		Stream:   true,
	}

	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q", req.Method)
	}
	if got := req.URL.String(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("url = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("accept = %q (want event-stream for streaming)", got)
	}

	var body anthropicOutboundRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Model != "claude-opus-4-8" {
		t.Errorf("model = %q", body.Model)
	}
	if body.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", body.MaxTokens, anthropicDefaultMaxTokens)
	}
	if body.System != "be brief" {
		t.Errorf("system = %q", body.System)
	}
	if !body.Stream {
		t.Error("stream should be true")
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != RoleUser {
		t.Fatalf("messages = %+v", body.Messages)
	}
	if len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0]["text"] != "hi" {
		t.Errorf("message content = %+v", body.Messages[0].Content)
	}
}

func TestAnthropicAdapter_BuildRequest_MaxTokensRespected(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelAnthropic, BaseURL: "https://api.anthropic.com"}
	mt := 256
	uni := UnifiedRequest{Model: "m", MaxTokens: &mt, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("x")}}}}

	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body anthropicOutboundRequest
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want 256", body.MaxTokens)
	}
	// non-streaming → no Accept event-stream header
	if got := req.Header.Get("Accept"); got == "text/event-stream" {
		t.Error("non-stream request should not set event-stream Accept")
	}
}

func TestAnthropicAdapter_BuildRequest_MissingBaseURL(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelAnthropic}
	if _, err := a.BuildRequest(context.Background(), UnifiedRequest{Model: "m"}, ch); err == nil {
		t.Fatal("expected error for missing base_url")
	}
}

func TestAnthropicAdapter_ParseResponse(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})
	respBody := `{
		"type":"message","role":"assistant","model":"claude-opus-4-8",
		"content":[{"type":"text","text":"hello"},{"type":"text","text":" world"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":7}
	}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(respBody))}

	out, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Text() != "hello\n world" {
		t.Errorf("text = %q", out.Text())
	}
	if out.StopReason != StopEndTurn {
		t.Errorf("stop = %q, want end_turn", out.StopReason)
	}
	if !usage.HasUpstream || usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.TotalTokens != 19 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestAnthropicAdapter_ParseResponse_UsageFallback(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})
	// No usage → estimate from completion text ("12345678" = 8 chars → 2 tokens).
	respBody := `{"type":"message","model":"m","content":[{"type":"text","text":"12345678"}],"stop_reason":"end_turn"}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(respBody))}

	_, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.HasUpstream {
		t.Error("usage should be estimated")
	}
	if usage.CompletionTokens != 2 {
		t.Errorf("estimated completion = %d, want 2", usage.CompletionTokens)
	}
}

func TestAnthropicAdapter_ParseResponse_HTTPError(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})
	resp := &http.Response{
		StatusCode: 401,
		Status:     "401 Unauthorized",
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)),
	}
	_, _, err := a.ParseResponse(resp)
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.StatusCode != 401 || ue.Message != "invalid x-api-key" {
		t.Errorf("UpstreamError = %+v", ue)
	}
}

// TestAnthropicAdapter_ParseStreamChunk feeds REAL Anthropic SSE data payloads
// (each is what readSSE yields after stripping "data: ") with eventType="".
func TestAnthropicAdapter_ParseStreamChunk(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})

	t.Run("message_start carries input tokens", func(t *testing.T) {
		p := `{"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":25,"output_tokens":1}}}`
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(p))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !meaningful || chunk.Usage == nil || chunk.Usage.PromptTokens != 25 {
			t.Errorf("chunk = %+v meaningful=%v", chunk, meaningful)
		}
	})

	t.Run("content_block_delta text_delta carries text", func(t *testing.T) {
		p := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(p))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !meaningful || chunk.Delta != "Hel" {
			t.Errorf("chunk = %+v meaningful=%v", chunk, meaningful)
		}
	})

	t.Run("thinking_delta is not meaningful", func(t *testing.T) {
		p := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"..."}}`
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(p))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if meaningful || chunk.Delta != "" {
			t.Errorf("thinking_delta should be skipped, got %+v meaningful=%v", chunk, meaningful)
		}
	})

	t.Run("message_delta carries stop_reason + output_tokens", func(t *testing.T) {
		p := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(p))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !meaningful || chunk.StopReason != StopEndTurn || chunk.Usage == nil || chunk.Usage.CompletionTokens != 42 {
			t.Errorf("chunk = %+v meaningful=%v", chunk, meaningful)
		}
	})

	t.Run("message_stop sets Done", func(t *testing.T) {
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(`{"type":"message_stop"}`))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !meaningful || !chunk.Done {
			t.Errorf("chunk = %+v meaningful=%v", chunk, meaningful)
		}
	})

	t.Run("ping is not meaningful", func(t *testing.T) {
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(`{"type":"ping"}`))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if meaningful {
			t.Errorf("ping should be skipped, got %+v", chunk)
		}
	})

	t.Run("content_block_start/stop are not meaningful", func(t *testing.T) {
		for _, p := range []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_stop","index":0}`,
		} {
			_, meaningful, err := a.ParseStreamChunk("", []byte(p))
			if err != nil {
				t.Fatalf("err on %s: %v", p, err)
			}
			if meaningful {
				t.Errorf("%s should be skipped", p)
			}
		}
	})

	t.Run("error event is fatal via UpstreamErr", func(t *testing.T) {
		p := `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`
		chunk, meaningful, err := a.ParseStreamChunk("", []byte(p))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !meaningful || chunk.UpstreamErr == nil {
			t.Fatalf("expected fatal UpstreamErr, got %+v", chunk)
		}
		if chunk.UpstreamErr.Message != "overloaded" || chunk.UpstreamErr.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("UpstreamErr = %+v", chunk.UpstreamErr)
		}
	})

	t.Run("malformed JSON returns skip error not panic", func(t *testing.T) {
		_, meaningful, err := a.ParseStreamChunk("", []byte(`{not json`))
		if err == nil {
			t.Error("expected decode error")
		}
		if meaningful {
			t.Error("malformed chunk should not be meaningful")
		}
	})
}
