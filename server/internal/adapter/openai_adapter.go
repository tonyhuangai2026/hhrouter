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
	// ToolCalls is set on an assistant turn that called tools (rendered from
	// unified tool_use blocks). ToolCallID is set on a role=tool result turn
	// (rendered from a unified tool_result block). Both omitempty so plain text
	// turns serialize byte-identically to before.
	ToolCalls  []openAIOutboundToolCall `json:"tool_calls,omitempty"`
	ToolCallID string                   `json:"tool_call_id,omitempty"`
}

// openAIOutboundToolCall is one element of an assistant message's tool_calls
// array sent upstream. Arguments is a JSON STRING (OpenAI's contract), so the
// canonical object input is marshaled and string-encoded when building it.
type openAIOutboundToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
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
	// Tools / ToolChoice are the OpenAI-shaped tool list and choice directive,
	// rendered from the canonical unified tools. omitempty keeps non-tool requests
	// byte-identical to before.
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
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
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
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
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
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
		msgs = append(msgs, openAIMessagesFromUnified(m)...)
	}

	body := openAIChatRequest{
		Model:       uni.Model,
		Messages:    msgs,
		Stream:      uni.Stream,
		MaxTokens:   uni.MaxTokens,
		Temperature: uni.Temperature,
		TopP:        uni.TopP,
		Stop:        uni.StopSequences,
		Tools:       canonicalToolsToOpenAI(uni.Tools),
		ToolChoice:  canonicalToolChoiceToOpenAI(uni.ToolChoice),
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
		if c.Message.Content != "" {
			out.Content = append(out.Content, TextBlock(c.Message.Content))
		}
		for _, tc := range c.Message.ToolCalls {
			out.Content = append(out.Content, ToolUseBlockOf(tc.ID, tc.Function.Name, openAIArgsToInput(tc.Function.Arguments)))
		}
		if len(out.Content) == 0 {
			// Preserve a (possibly empty) text block so Text() and downstream
			// builders behave as before for an empty completion.
			out.Content = []ContentBlock{TextBlock("")}
		}
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
		if len(c.Delta.ToolCalls) > 0 {
			tc := c.Delta.ToolCalls[0]
			out.ToolCallDelta = &ToolCallDelta{
				Index:        tc.Index,
				ID:           tc.ID,
				Name:         tc.Function.Name,
				ArgsFragment: tc.Function.Arguments,
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			out.StopReason = openAIFinishToStop(*c.FinishReason)
		}
	}
	meaningful := out.Delta != "" || out.StopReason != StopUnknown || out.Usage != nil || out.ToolCallDelta != nil
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

// openAIMessagesFromUnified renders one unified message into one or more OpenAI
// chat messages. A plain text/image turn maps to a single message (byte-identical
// to before). A tool_result block becomes its OWN role=tool message (OpenAI's
// contract requires tool results as standalone messages). An assistant turn with
// tool_use blocks carries them in tool_calls alongside any text content.
func openAIMessagesFromUnified(m Message) []openAIChatMessage {
	// Split content into tool_result blocks (own messages) vs the rest.
	var toolResults []ContentBlock
	var rest []ContentBlock
	var toolCalls []openAIOutboundToolCall
	for _, c := range m.Content {
		switch {
		case c.IsToolResult():
			toolResults = append(toolResults, c)
		case c.IsToolUse():
			tc := openAIOutboundToolCall{ID: c.ToolUse.ID, Type: "function"}
			tc.Function.Name = c.ToolUse.Name
			tc.Function.Arguments = inputToOpenAIArgs(c.ToolUse.Input)
			toolCalls = append(toolCalls, tc)
		default:
			rest = append(rest, c)
		}
	}

	var out []openAIChatMessage

	// tool_result blocks → one role=tool message each.
	for _, tr := range toolResults {
		out = append(out, openAIChatMessage{
			Role:       "tool",
			ToolCallID: tr.ToolResult.ToolUseID,
			Content:    toolResultContentToText(tr.ToolResult.Content),
		})
	}

	// The main message (text/images + any assistant tool_calls). Skip emitting it
	// when this turn was ONLY tool_result blocks (already emitted above).
	if len(rest) > 0 || len(toolCalls) > 0 || len(toolResults) == 0 {
		msg := openAIChatMessage{Role: m.Role}
		restMsg := Message{Role: m.Role, Content: rest}
		if len(rest) > 0 {
			msg.Content = openAIOutboundContent(restMsg)
		} else if len(toolCalls) > 0 {
			// An assistant turn that is purely tool calls has null content per the
			// OpenAI contract.
			msg.Content = nil
		} else {
			msg.Content = ""
		}
		msg.ToolCalls = toolCalls
		out = append(out, msg)
	}
	return out
}

// toolResultContentToText renders a canonical tool_result content (raw JSON,
// usually a JSON string or an array of blocks) into the string OpenAI's tool
// message content expects. A JSON string is unquoted; anything else is passed
// through as compact JSON text.
func toolResultContentToText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return s
}

// inputToOpenAIArgs renders a canonical tool_use input (JSON object) into the
// JSON-STRING form OpenAI's tool_calls.function.arguments requires. Empty input
// becomes "{}".
func inputToOpenAIArgs(input json.RawMessage) string {
	s := strings.TrimSpace(string(input))
	if s == "" {
		return "{}"
	}
	return s
}

// canonicalToolsToOpenAI converts the canonical (Anthropic-shaped) tools array
// ([{name,description,input_schema}]) into the OpenAI tools array
// ([{type:"function",function:{name,description,parameters}}]). Returns nil when
// there are no tools.
func canonicalToolsToOpenAI(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(strings.TrimSpace(string(t.InputSchema))) > 0 {
			fn["parameters"] = t.InputSchema
		} else {
			fn["parameters"] = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// canonicalToolChoiceToOpenAI maps the canonical (Anthropic) tool_choice to the
// OpenAI tool_choice. {type:auto}→"auto", {type:none}→"none", {type:any}→
// "required", {type:tool,name}→{type:function,function:{name}}. nil/unknown → nil.
func canonicalToolChoiceToOpenAI(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var c struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	switch c.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "tool":
		if c.Name != "" {
			b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": c.Name}})
			return b
		}
	}
	return nil
}

// promptTextFromResponse can't recover the prompt from a response (it isn't
// echoed), so it returns an empty string; the estimate then reflects only the
// completion side. This keeps the fallback total non-zero when at least the
// completion is present.
func promptTextFromResponse(openAIChatResponse) string { return "" }
