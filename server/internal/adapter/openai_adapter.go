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

// OpenAIAdapter forwards unified requests to an OpenAI-compatible upstream's
// Chat Completions endpoint (Tech Design §6): POST {base_url}/v1/chat/completions
// with header `Authorization: Bearer {decrypted channel.key}`. Streaming is
// requested via the `stream: true` body flag (SSE), so the same path is used for
// both streaming and non-streaming.
type OpenAIAdapter struct {
	dec Decryptor
}

// NewOpenAIAdapter constructs an OpenAIAdapter using dec to obtain the channel
// credential at request-build time.
func NewOpenAIAdapter(dec Decryptor) *OpenAIAdapter { return &OpenAIAdapter{dec: dec} }

// Name implements Adapter.
func (a *OpenAIAdapter) Name() string { return "openai" }

// ---- wire types (OpenAI Chat Completions) -------------------------------

// openAIChatMessage is one message in the chat/completions request/response.
// Content is `any` so a request message can carry either a plain string
// (text-only — the legacy, byte-identical shape) or an array of multimodal parts
// ([]openAIOutboundPart) when the turn includes images. Response messages decode
// into a string (OpenAI replies are text), which `any` accepts.
type openAIChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// openAIOutboundPart is one element of an OpenAI multimodal request content
// array: a {type:"text",text} part or a {type:"image_url",image_url:{url}} part.
type openAIOutboundPart struct {
	Type     string                  `json:"type"`
	Text     string                  `json:"text,omitempty"`
	ImageURL *openAIOutboundImageURL `json:"image_url,omitempty"`
}

type openAIOutboundImageURL struct {
	URL string `json:"url"`
}

// openAIChatRequest is the request body sent to {base_url}/v1/chat/completions.
type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	Stream      bool                `json:"stream,omitempty"`
	MaxTokens   *int                `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	TopP        *float64            `json:"top_p,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	// StreamOptions.IncludeUsage asks compatible upstreams to emit a final usage
	// chunk in the stream (OpenAI behavior). Best-effort: upstreams that don't
	// support it ignore it, and usage.go falls back to estimation.
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIUsage is the usage object returned by chat/completions. prompt_tokens
// already INCLUDES cached tokens; prompt_tokens_details.cached_tokens breaks out
// the cache-hit portion so it can be priced on the cache-read tier. (OpenAI has
// no separate cache-write count.)
type openAIUsage struct {
	PromptTokens        int                       `json:"prompt_tokens"`
	CompletionTokens    int                       `json:"completion_tokens"`
	TotalTokens         int                       `json:"total_tokens"`
	PromptTokensDetails *openAIPromptTokensDetail `json:"prompt_tokens_details,omitempty"`
}

// openAIPromptTokensDetail carries the cache-hit breakdown of prompt_tokens.
type openAIPromptTokensDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

// openAIRespMessage is one message in a chat/completions RESPONSE. OpenAI
// replies carry text content, so this decodes content as a plain string
// (separate from the request-side openAIChatMessage whose Content is `any`).
type openAIRespMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIChatResponse is the non-streaming chat/completions response.
type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      openAIRespMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// openAIStreamChunk is one SSE `data:` payload from a streaming response.
type openAIStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

// ---- BuildRequest --------------------------------------------------------

// BuildRequest implements Adapter. It marshals the unified request into the
// OpenAI Chat Completions body, prepending the merged system prompt as a leading
// system message (the OpenAI convention).
func (a *OpenAIAdapter) BuildRequest(ctx context.Context, uni UnifiedRequest, ch *model.Channel) (*http.Request, error) {
	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("openai adapter: channel %d has no base_url", ch.ID)
	}
	key, err := a.dec.Decrypt(ch)
	if err != nil {
		return nil, fmt.Errorf("openai adapter: decrypt key: %w", err)
	}

	msgs := make([]openAIChatMessage, 0, len(uni.Messages)+1)
	if strings.TrimSpace(uni.System) != "" {
		msgs = append(msgs, openAIChatMessage{Role: RoleSystem, Content: uni.System})
	}
	for _, m := range uni.Messages {
		msgs = append(msgs, openAIChatMessage{Role: m.Role, Content: openAIOutboundContent(m)})
	}

	body := openAIChatRequest{
		Model:       uni.Model,
		Messages:    msgs,
		Stream:      uni.Stream,
		MaxTokens:   uni.MaxTokens,
		Temperature: uni.Temperature,
		TopP:        uni.TopP,
		Stop:        uni.StopSequences,
	}
	if uni.Stream {
		body.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai adapter: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if uni.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// ---- ParseResponse -------------------------------------------------------

// ParseResponse implements Adapter for the non-streaming case.
func (a *OpenAIAdapter) ParseResponse(resp *http.Response) (UnifiedResponse, Usage, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider:   a.Name(),
			StatusCode: resp.StatusCode,
			Message:    openAIErrorMessage(raw, resp.Status),
			Body:       snippet(raw),
		}
	}

	var parsed openAIChatResponse
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

	out := UnifiedResponse{Model: parsed.Model, StopReason: StopUnknown}
	if len(parsed.Choices) > 0 {
		c := parsed.Choices[0]
		out.Content = []ContentBlock{TextBlock(c.Message.Content)}
		out.StopReason = openAIFinishToStop(c.FinishReason)
	}

	usage := usageFromOpenAI(parsed.Usage)
	if !usage.HasUpstream {
		// Fall back to a char-based estimate when the upstream omitted usage.
		usage = estimateUsageFromText(promptTextFromResponse(parsed), out.Text())
	}
	out.Usage = usage
	return out, usage, nil
}

// ParseStreamChunk implements Adapter. OpenAI is a plain-SSE upstream with no
// event-stream header, so eventType is always "" here and is ignored; payload is
// the bytes after the SSE "data: " prefix for one event. The terminal "[DONE]"
// sentinel returns Done=true.
func (a *OpenAIAdapter) ParseStreamChunk(_ string, payload []byte) (StreamChunk, bool, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return StreamChunk{}, false, nil
	}
	if string(trimmed) == "[DONE]" {
		return StreamChunk{Done: true}, true, nil
	}

	var chunk openAIStreamChunk
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return StreamChunk{}, false, fmt.Errorf("openai adapter: decode stream chunk: %w", err)
	}

	out := StreamChunk{Model: chunk.Model}
	if u := usageFromOpenAI(chunk.Usage); u.HasUpstream {
		out.Usage = &u
	}
	if len(chunk.Choices) > 0 {
		c := chunk.Choices[0]
		out.Delta = c.Delta.Content
		if c.FinishReason != nil && *c.FinishReason != "" {
			out.StopReason = openAIFinishToStop(*c.FinishReason)
		}
	}
	meaningful := out.Delta != "" || out.StopReason != StopUnknown || out.Usage != nil
	return out, meaningful, nil
}

// openAIErrorMessage extracts a readable message from an OpenAI error body,
// falling back to the HTTP status line.
func openAIErrorMessage(raw []byte, status string) string {
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

// openAIOutboundContent renders a unified message's content into the OpenAI
// request `content` field. When the message has no image blocks it returns a
// plain string (Message.Text()) — byte-identical to the legacy text-only path.
// When images are present it returns a []openAIOutboundPart array: text blocks →
// {type:"text",text}; url images → {type:"image_url",image_url:{url}}; base64
// images → {type:"image_url",image_url:{url:"data:<mt>;base64,<data>"}}.
func openAIOutboundContent(m Message) any {
	if !m.HasImages() {
		return m.Text()
	}
	parts := make([]openAIOutboundPart, 0, len(m.Content))
	for _, c := range m.Content {
		if c.IsImage() {
			url := c.Image.URL
			if c.Image.Kind == ImageKindBase64 {
				url = buildDataURL(c.Image.MediaType, c.Image.Data)
			}
			parts = append(parts, openAIOutboundPart{Type: "image_url", ImageURL: &openAIOutboundImageURL{URL: url}})
			continue
		}
		if c.Text != "" {
			parts = append(parts, openAIOutboundPart{Type: "text", Text: c.Text})
		}
	}
	return parts
}

// promptTextFromResponse can't recover the prompt from a response (it isn't
// echoed), so it returns an empty string; the estimate then reflects only the
// completion side. This keeps the fallback total non-zero when at least the
// completion is present.
func promptTextFromResponse(openAIChatResponse) string { return "" }
