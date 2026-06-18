package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agent-router/server/internal/model"
)

// anthropicVersion is the Anthropic API version header value sent on every
// upstream request. Pinned to the long-stable GA value.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is sent when the unified request did not specify a
// max_tokens. The Anthropic Messages API REQUIRES max_tokens, so the adapter
// must always send a value or the upstream returns a 400.
const anthropicDefaultMaxTokens = 4096

// AnthropicAdapter forwards unified requests to the native Anthropic Messages
// API: POST {base_url}/v1/messages with headers `x-api-key: {decrypted key}`
// and `anthropic-version: 2023-06-01`. Streaming is requested via the
// `stream: true` body flag (plain SSE), so the same endpoint serves both
// streaming and non-streaming.
//
// The outbound request body reuses the existing unified→Anthropic content
// transform (BuildAnthropicContentBlocks) and the stop-reason mapping
// (anthropicStopToUnified) from transform.go. Parsing the UPSTREAM response and
// stream is implemented here (the BuildAnthropic* helpers in transform.go build
// the INBOUND responses this platform returns to its own clients, a different
// direction).
type AnthropicAdapter struct {
	dec Decryptor
}

// NewAnthropicAdapter constructs an AnthropicAdapter using dec to obtain the
// channel credential at request-build time.
func NewAnthropicAdapter(dec Decryptor) *AnthropicAdapter { return &AnthropicAdapter{dec: dec} }

// Name implements Adapter.
func (a *AnthropicAdapter) Name() string { return "anthropic" }

// ---- wire types (Anthropic Messages) ------------------------------------

// anthropicOutboundRequest is the request body sent to {base_url}/v1/messages.
type anthropicOutboundRequest struct {
	Model         string                   `json:"model"`
	MaxTokens     int                      `json:"max_tokens"`
	System        string                   `json:"system,omitempty"`
	Messages      []anthropicOutboundMsg   `json:"messages"`
	Stream        bool                     `json:"stream,omitempty"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
}

type anthropicOutboundMsg struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// anthropicUsage is the usage object in non-stream responses and message_start /
// message_delta stream events. input_tokens EXCLUDES cached tokens; the cache
// buckets are reported separately so they can be priced on their own tiers:
// cache_read_input_tokens = cache hits, cache_creation_input_tokens = tokens
// written into the cache this turn. Both absent (older API / no caching) → 0.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// anthropicMessageResponse is the non-streaming /v1/messages response.
type anthropicMessageResponse struct {
	Type    string `json:"type"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *anthropicUsage `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicStreamEvent is one decoded SSE `data:` payload. Anthropic SSE
// payloads are self-describing via the `type` field; this struct is a superset
// covering message_start / content_block_delta / message_delta / message_stop /
// error and ignores the framing-only events (ping / content_block_start/stop).
type anthropicStreamEvent struct {
	Type string `json:"type"`
	// message_start carries the initial usage (input_tokens).
	Message *struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	// content_block_delta carries delta.{type,text}; message_delta carries
	// delta.stop_reason.
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	// message_delta carries the cumulative output_tokens.
	Usage *anthropicUsage `json:"usage"`
	// error events carry error.{type,message}.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---- BuildRequest --------------------------------------------------------

// BuildRequest implements Adapter. It marshals the unified request into the
// Anthropic Messages body, hoisting the merged system prompt into the top-level
// `system` field and defaulting max_tokens when the caller did not set it.
func (a *AnthropicAdapter) BuildRequest(ctx context.Context, uni UnifiedRequest, ch *model.Channel) (*http.Request, error) {
	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("anthropic adapter: channel %d has no base_url", ch.ID)
	}
	key, err := a.dec.Decrypt(ch)
	if err != nil {
		return nil, fmt.Errorf("anthropic adapter: decrypt key: %w", err)
	}

	maxTokens := anthropicDefaultMaxTokens
	if uni.MaxTokens != nil && *uni.MaxTokens > 0 {
		maxTokens = *uni.MaxTokens
	}

	msgs := make([]anthropicOutboundMsg, 0, len(uni.Messages))
	for _, m := range uni.Messages {
		msgs = append(msgs, anthropicOutboundMsg{
			Role:    m.Role,
			Content: BuildAnthropicContentBlocks(m.Content),
		})
	}

	body := anthropicOutboundRequest{
		Model:         uni.Model,
		MaxTokens:     maxTokens,
		System:        strings.TrimSpace(uni.System),
		Messages:      msgs,
		Stream:        uni.Stream,
		Temperature:   uni.Temperature,
		TopP:          uni.TopP,
		StopSequences: uni.StopSequences,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic adapter: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	if uni.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// ---- ParseResponse -------------------------------------------------------

// ParseResponse implements Adapter for the non-streaming case.
func (a *AnthropicAdapter) ParseResponse(resp *http.Response) (UnifiedResponse, Usage, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider:   a.Name(),
			StatusCode: resp.StatusCode,
			Message:    anthropicErrorMessage(raw, resp.Status),
			Body:       snippet(raw),
		}
	}

	var parsed anthropicMessageResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider: a.Name(),
			Message:  fmt.Sprintf("decode response: %v", err),
			Body:     snippet(raw),
		}
	}
	if parsed.Error != nil {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider:   a.Name(),
			StatusCode: resp.StatusCode,
			Message:    parsed.Error.Message,
			Body:       snippet(raw),
		}
	}

	out := UnifiedResponse{Model: parsed.Model, StopReason: anthropicStopToUnified(parsed.StopReason)}
	for _, c := range parsed.Content {
		if c.Type == "text" && c.Text != "" {
			out.Content = append(out.Content, TextBlock(c.Text))
		}
	}

	usage := usageFromAnthropic(parsed.Usage)
	if !usage.HasUpstream {
		usage = estimateUsageFromText("", out.Text())
	}
	out.Usage = usage
	return out, usage, nil
}

// ---- ParseStreamChunk ----------------------------------------------------

// ParseStreamChunk implements Adapter. Anthropic is a plain-SSE upstream, so
// eventType is always "" and is ignored; payload is the bytes after the SSE
// "data: " prefix for one event. The payload is self-describing via its `type`
// field. message_start carries input_tokens; content_block_delta with a
// text_delta carries the incremental text; message_delta carries the stop
// reason + output_tokens; message_stop ends the stream; an error event is a
// FATAL upstream error surfaced via StreamChunk.UpstreamErr; framing-only events
// (ping / content_block_start / content_block_stop) are not meaningful.
func (a *AnthropicAdapter) ParseStreamChunk(_ string, payload []byte) (StreamChunk, bool, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return StreamChunk{}, false, nil
	}

	var ev anthropicStreamEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		// Malformed frame: skip it (not fatal), matching the adapter contract.
		return StreamChunk{}, false, fmt.Errorf("anthropic adapter: decode stream chunk: %w", err)
	}

	switch ev.Type {
	case "message_start":
		var out StreamChunk
		if ev.Message != nil {
			out.Model = ev.Message.Model
			if ev.Message.Usage != nil {
				// Cache tokens (read + creation) arrive on message_start alongside
				// input_tokens; carry them so the relay can price the cache tiers.
				u := Usage{
					PromptTokens:     ev.Message.Usage.InputTokens,
					CacheReadTokens:  ev.Message.Usage.CacheReadInputTokens,
					CacheWriteTokens: ev.Message.Usage.CacheCreationInputTokens,
					HasUpstream:      true,
				}
				out.Usage = &u
			}
		}
		return out, out.Usage != nil, nil

	case "content_block_delta":
		if ev.Delta != nil && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			return StreamChunk{Delta: ev.Delta.Text}, true, nil
		}
		// thinking_delta / input_json_delta / empty → nothing to emit.
		return StreamChunk{}, false, nil

	case "message_delta":
		out := StreamChunk{}
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			out.StopReason = anthropicStopToUnified(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			u := Usage{CompletionTokens: ev.Usage.OutputTokens, HasUpstream: true}
			out.Usage = &u
		}
		meaningful := out.StopReason != StopUnknown || out.Usage != nil
		return out, meaningful, nil

	case "message_stop":
		return StreamChunk{Done: true}, true, nil

	case "error":
		msg := "anthropic upstream stream error"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		ue := &UpstreamError{
			Provider:   a.Name(),
			StatusCode: anthropicStreamErrorStatus(ev.Error),
			Message:    msg,
		}
		return StreamChunk{UpstreamErr: ue}, true, nil

	default:
		// ping / content_block_start / content_block_stop / unknown → skip.
		return StreamChunk{}, false, nil
	}
}

// usageFromAnthropic converts an Anthropic usage object to Usage. A nil pointer
// yields a zero Usage with HasUpstream=false.
func usageFromAnthropic(u *anthropicUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		HasUpstream:      true,
	}
	return out.normalize()
}

// anthropicErrorMessage extracts a readable message from an Anthropic error
// body, falling back to the HTTP status line.
func anthropicErrorMessage(raw []byte, status string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return status
}

// anthropicStreamErrorStatus maps an Anthropic stream `error` event to an HTTP
// status for the relay's error outcome. Anthropic stream errors carry a type
// string (e.g. "overloaded_error", "rate_limit_error"); map the common ones,
// defaulting to 502 Bad Gateway.
func anthropicStreamErrorStatus(e *struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}) int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch e.Type {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return http.StatusServiceUnavailable
	case "api_error":
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}
