package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// transform.go converts between the two INBOUND wire formats this platform
// exposes — OpenAI Chat Completions and Anthropic Messages — and the unified
// representation, and between the unified representation and the Bedrock Converse
// shape used by the bedrock adapter. It also converts a unified StreamChunk into
// each inbound format's streaming chunk.
//
// Direction summary (Tech Design §6):
//
//	inbound OpenAI Chat  ⇄ UnifiedRequest/Response  ⇄ upstream (any adapter)
//	inbound Anthropic    ⇄ UnifiedRequest/Response  ⇄ upstream (any adapter)
//	UnifiedRequest/Resp  ⇄ Bedrock Converse  (used inside bedrock_adapter.go)
//
// System handling: OpenAI carries system as a message with role=system; Anthropic
// and Bedrock carry it as a dedicated top-level field. ParseOpenAIRequest hoists
// system messages out of the message list into UnifiedRequest.System (joined with
// newlines); the inbound-Anthropic / outbound-Bedrock paths merge the system
// blocks the same way.

// ========================= Inbound OpenAI Chat ============================

// OpenAIChatInbound is the inbound /v1/chat/completions request body. Content
// may be a plain string or an array of typed parts ({"type":"text","text":...}),
// both of which are normalized to text.
type OpenAIChatInbound struct {
	Model       string                 `json:"model"`
	Messages    []OpenAIInboundMessage `json:"messages"`
	Stream      bool                   `json:"stream,omitempty"`
	MaxTokens   *int                   `json:"max_tokens,omitempty"`
	Temperature *float64               `json:"temperature,omitempty"`
	TopP        *float64               `json:"top_p,omitempty"`
	Stop        []string               `json:"stop,omitempty"`
}

// OpenAIInboundMessage allows content to be a string or a parts array.
type OpenAIInboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// openAIContentToText flattens an OpenAI message content field (string or parts)
// to plain text, discarding non-text parts. Retained for system-message
// flattening (system is text-only). For user/assistant turns use
// openAIContentToBlocks, which preserves image parts.
func openAIContentToText(raw json.RawMessage) string {
	var b strings.Builder
	for _, blk := range openAIContentToBlocks(raw) {
		if blk.Type == BlockText && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// openAIPart is one element of an OpenAI multimodal content array. text parts
// carry Text; image_url parts carry ImageURL.URL (either an http(s) URL or a
// `data:<mt>;base64,...` URL).
type openAIPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// openAIContentToBlocks parses an OpenAI message content field (a plain string
// or an array of typed parts) into unified ContentBlocks. A plain string maps to
// a single text block. {type:"text"} → text block; {type:"image_url"} → image
// block (a `data:<mt>;base64,` URL is split into base64 Data + MediaType with
// Kind=base64, otherwise Kind=url). Unknown part types are skipped.
//
// Returns nil for empty content. Note: text-only content always produces exactly
// the same single text block as the legacy text path, so downstream behavior is
// unchanged for non-image messages.
func openAIContentToBlocks(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	// Try a plain string first (the common case).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentBlock{TextBlock(s)}
	}
	// Else an array of typed parts.
	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			out = append(out, imageBlockFromURL(p.ImageURL.URL))
		default: // "text" and any text-bearing part
			if p.Text != "" {
				out = append(out, TextBlock(p.Text))
			}
		}
	}
	return out
}

// imageBlockFromURL builds a unified image block from any image URL string:
// a base64 data URL becomes a base64 image block (kind=base64) with the parsed
// media type; anything else becomes a url image block (kind=url).
func imageBlockFromURL(url string) ContentBlock {
	if mt, data, ok := parseDataURL(url); ok {
		return ImageBase64Block(mt, data)
	}
	return ImageURLBlock(url)
}

// ParseOpenAIRequest converts an inbound OpenAI Chat Completions request into a
// UnifiedRequest, hoisting and merging any system messages.
func ParseOpenAIRequest(in OpenAIChatInbound) UnifiedRequest {
	uni := UnifiedRequest{
		Model:         in.Model,
		Stream:        in.Stream,
		MaxTokens:     in.MaxTokens,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		StopSequences: in.Stop,
	}
	var systems []string
	for _, m := range in.Messages {
		if m.Role == RoleSystem {
			// System is text-only; flatten any parts to text.
			if text := openAIContentToText(m.Content); text != "" {
				systems = append(systems, text)
			}
			continue
		}
		blocks := openAIContentToBlocks(m.Content)
		if len(blocks) == 0 {
			// Preserve the legacy shape: an empty/absent content still yields one
			// (empty) text block so message count and Text() are unchanged.
			blocks = []ContentBlock{TextBlock("")}
		}
		uni.Messages = append(uni.Messages, Message{Role: m.Role, Content: blocks})
	}
	uni.System = strings.Join(systems, "\n")
	return uni
}

// BuildOpenAIResponse converts a UnifiedResponse into an inbound OpenAI Chat
// Completions response body (id/created are filled by the relay).
func BuildOpenAIResponse(r UnifiedResponse) map[string]any {
	return map[string]any{
		"object": "chat.completion",
		"model":  r.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": RoleAssistant, "content": r.Text()},
				"finish_reason": stopToOpenAIFinish(r.StopReason),
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     r.Usage.PromptTokens,
			"completion_tokens": r.Usage.CompletionTokens,
			"total_tokens":      r.Usage.TotalTokens,
		},
	}
}

// BuildOpenAIStreamChunk converts a unified StreamChunk into an inbound OpenAI
// SSE chunk object (the value that follows "data: "). Returns nil for the [DONE]
// sentinel so the caller can emit the literal "data: [DONE]" instead.
func BuildOpenAIStreamChunk(model string, c StreamChunk) map[string]any {
	if c.Done && c.Delta == "" && c.StopReason == StopUnknown && c.Usage == nil {
		return nil
	}
	delta := map[string]any{}
	if c.Delta != "" {
		delta["content"] = c.Delta
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if c.StopReason != StopUnknown {
		choice["finish_reason"] = stopToOpenAIFinish(c.StopReason)
	} else {
		choice["finish_reason"] = nil
	}
	out := map[string]any{
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []map[string]any{choice},
	}
	if c.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     c.Usage.PromptTokens,
			"completion_tokens": c.Usage.CompletionTokens,
			"total_tokens":      c.Usage.TotalTokens,
		}
	}
	return out
}

// ========================= Inbound Anthropic Messages =====================

// AnthropicInbound is the inbound /v1/messages request body. System may be a
// string or an array of {type:text,text} blocks; message content may be a string
// or an array of content blocks.
type AnthropicInbound struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Stream        bool               `json:"stream,omitempty"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

// AnthropicMessage is one inbound message; Content is a string or block array.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicInboundBlock is one Anthropic content block. text blocks carry Text;
// image blocks carry Source (a base64 source with media_type+data, or a url
// source with url).
type anthropicInboundBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"` // "base64" | "url"
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// anthropicContentToBlocks flattens Anthropic content (string or block array)
// into unified ContentBlocks, preserving image blocks. A plain string maps to a
// single text block. {type:"image", source:{type:"base64",media_type,data}} →
// base64 image block; source.type=="url" → url image block.
func anthropicContentToBlocks(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentBlock{TextBlock(s)}
	}
	var blocks []anthropicInboundBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "image":
			if b.Source == nil {
				continue
			}
			switch b.Source.Type {
			case "url":
				if b.Source.URL != "" {
					out = append(out, ImageURLBlock(b.Source.URL))
				}
			default: // "base64"
				if b.Source.Data != "" {
					out = append(out, ImageBase64Block(b.Source.MediaType, b.Source.Data))
				}
			}
		case "text", "":
			if b.Text != "" {
				out = append(out, TextBlock(b.Text))
			}
		default:
			if b.Text != "" {
				out = append(out, TextBlock(b.Text))
			}
		}
	}
	return out
}

// anthropicSystemToText flattens the Anthropic system field (string or block
// array) into a single merged string.
func anthropicSystemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ParseAnthropicRequest converts an inbound Anthropic Messages request into a
// UnifiedRequest.
func ParseAnthropicRequest(in AnthropicInbound) UnifiedRequest {
	uni := UnifiedRequest{
		Model:         in.Model,
		System:        anthropicSystemToText(in.System),
		Stream:        in.Stream,
		MaxTokens:     in.MaxTokens,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		StopSequences: in.StopSequences,
	}
	// Anthropic's contract carries system in the top-level `system` field, but some
	// clients (e.g. Claude Code) also slip a role="system" entry into messages.
	// Upstreams reject any messages[].role that is not user/assistant, so hoist
	// those out into System (mirroring the OpenAI inbound path) rather than
	// forwarding them verbatim.
	var systems []string
	if s := strings.TrimSpace(uni.System); s != "" {
		systems = append(systems, s)
	}
	for _, m := range in.Messages {
		if m.Role == RoleSystem {
			// System is text-only; reuse the string/block-array flattener.
			if text := strings.TrimSpace(anthropicSystemToText(m.Content)); text != "" {
				systems = append(systems, text)
			}
			continue
		}
		uni.Messages = append(uni.Messages, Message{Role: m.Role, Content: anthropicContentToBlocks(m.Content)})
	}
	uni.System = strings.Join(systems, "\n")
	return uni
}

// BuildAnthropicContentBlock converts a single unified ContentBlock into the
// Anthropic Messages wire shape: a text block → {type:"text",text}; a base64
// image → {type:"image",source:{type:"base64",media_type,data}}; a url image →
// {type:"image",source:{type:"url",url}}. This is the outbound counterpart of
// anthropicContentToBlocks, provided for symmetry and for callers that build an
// Anthropic-format request from the unified representation (e.g. the test-chat
// path when the upstream speaks Anthropic Messages). ok=false for an image block
// with no usable source.
func BuildAnthropicContentBlock(c ContentBlock) (block map[string]any, ok bool) {
	if c.IsImage() {
		switch c.Image.Kind {
		case ImageKindBase64:
			if c.Image.Data == "" {
				return nil, false
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": c.Image.MediaType,
					"data":       c.Image.Data,
				},
			}, true
		case ImageKindURL:
			if c.Image.URL == "" {
				return nil, false
			}
			return map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "url", "url": c.Image.URL},
			}, true
		default:
			return nil, false
		}
	}
	return map[string]any{"type": "text", "text": c.Text}, true
}

// BuildAnthropicContentBlocks converts a unified message's content into an
// Anthropic content-block array (text + image), skipping unusable image blocks.
func BuildAnthropicContentBlocks(content []ContentBlock) []map[string]any {
	out := make([]map[string]any, 0, len(content))
	for _, c := range content {
		if block, ok := BuildAnthropicContentBlock(c); ok {
			out = append(out, block)
		}
	}
	return out
}

// BuildAnthropicResponse converts a UnifiedResponse into an inbound Anthropic
// Messages response body (id is filled by the relay).
func BuildAnthropicResponse(r UnifiedResponse) map[string]any {
	content := make([]map[string]any, 0, len(r.Content))
	for _, c := range r.Content {
		content = append(content, map[string]any{"type": "text", "text": c.Text})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{
		"type":        "message",
		"role":        RoleAssistant,
		"model":       r.Model,
		"content":     content,
		"stop_reason": stopToAnthropic(r.StopReason),
		"usage": map[string]any{
			"input_tokens":  r.Usage.PromptTokens,
			"output_tokens": r.Usage.CompletionTokens,
		},
	}
}

// BuildAnthropicStreamEvent converts a unified StreamChunk into an inbound
// Anthropic SSE event. It returns the event name and a JSON-serializable payload.
// Text deltas map to content_block_delta; the terminating chunk maps to
// message_delta (carrying stop_reason / usage) — the relay is expected to emit
// the surrounding message_start / content_block_start / message_stop framing.
// ok=false signals a chunk with nothing to emit.
func BuildAnthropicStreamEvent(c StreamChunk) (event string, payload map[string]any, ok bool) {
	switch {
	case c.Delta != "":
		return "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": c.Delta},
		}, true
	case c.StopReason != StopUnknown || c.Usage != nil:
		p := map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopToAnthropic(c.StopReason)},
		}
		if c.Usage != nil {
			p["usage"] = map[string]any{"output_tokens": c.Usage.CompletionTokens}
		}
		return "message_delta", p, true
	default:
		return "", nil, false
	}
}

// BuildBedrockResponse converts a UnifiedResponse into a Bedrock Converse
// (non-streaming) response body, for keys that pin output_format=bedrock. Shape:
//
//	{output:{message:{role,content:[{text}]}}, stopReason, usage:{inputTokens,outputTokens,totalTokens}}
//
// Like BuildAnthropicResponse, an empty content list yields a single {text:""}
// block so the message is always well-formed. Text is taken from r.Text() (which
// concatenates the unified text blocks) — the MVP renders assistant output as a
// single text content block, matching the rest of the platform's text-only path.
func BuildBedrockResponse(r UnifiedResponse) map[string]any {
	text := r.Text()
	return map[string]any{
		"output": map[string]any{
			"message": map[string]any{
				"role":    RoleAssistant,
				"content": []map[string]any{{"text": text}},
			},
		},
		"stopReason": stopToBedrock(r.StopReason),
		"usage": map[string]any{
			"inputTokens":  r.Usage.PromptTokens,
			"outputTokens": r.Usage.CompletionTokens,
			"totalTokens":  r.Usage.TotalTokens,
		},
	}
}

// Bedrock ConverseStream event-type discriminators (the :event-type header
// value the relay stamps via EncodeBedrockFrame).
const (
	BedrockEventMessageStart     = "messageStart"
	BedrockEventContentBlockDelta = "contentBlockDelta"
	BedrockEventContentBlockStop  = "contentBlockStop"
	BedrockEventMessageStop       = "messageStop"
	BedrockEventMetadata          = "metadata"
)

// BedrockMessageStartPayload is the UNWRAPPED messageStart event body. Bedrock
// sends role on the opening event.
func BedrockMessageStartPayload() map[string]any {
	return map[string]any{"role": RoleAssistant}
}

// BedrockContentBlockStartPayload is the UNWRAPPED contentBlockStart body for the
// single text block (index 0). Bedrock opens a content block before its deltas.
func BedrockContentBlockStartPayload() map[string]any {
	return map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}
}

// BuildBedrockStreamEvent maps a unified text-delta StreamChunk to the
// contentBlockDelta event payload (UNWRAPPED inner JSON). ok=false when the
// chunk carries no text delta (stop/usage are emitted by the relay as explicit
// contentBlockStop/messageStop/metadata events at the right lifecycle points).
func BuildBedrockStreamEvent(c StreamChunk) (eventType string, payload map[string]any, ok bool) {
	if c.Delta != "" {
		return BedrockEventContentBlockDelta, map[string]any{
			"contentBlockIndex": 0,
			"delta":             map[string]any{"text": c.Delta},
		}, true
	}
	return "", nil, false
}

// BedrockContentBlockStopPayload is the UNWRAPPED contentBlockStop body (closes
// the single text block).
func BedrockContentBlockStopPayload() map[string]any {
	return map[string]any{"contentBlockIndex": 0}
}

// BedrockMessageStopPayload is the UNWRAPPED messageStop body, carrying the
// normalized stop reason mapped to the Bedrock vocabulary.
func BedrockMessageStopPayload(stop StopReason) map[string]any {
	return map[string]any{"stopReason": stopToBedrock(stop)}
}

// BedrockMetadataPayload is the UNWRAPPED metadata (terminal usage) body.
func BedrockMetadataPayload(u Usage) map[string]any {
	return map[string]any{
		"usage": map[string]any{
			"inputTokens":  u.PromptTokens,
			"outputTokens": u.CompletionTokens,
			"totalTokens":  u.TotalTokens,
		},
	}
}

// ========================= Unified ⇄ Bedrock Converse =====================

// unifiedToBedrock converts a UnifiedRequest into a Bedrock Converse request
// body. System is mapped to the top-level system block array; sampling params to
// inferenceConfig. Bedrock only accepts user/assistant roles.
//
// Image handling: Converse accepts only inline base64 bytes, so a base64 image
// block maps directly to {image:{format,source:{bytes}}}, while a url image
// block is downloaded to base64 first (size-capped, with a timeout). A download
// failure aborts the whole conversion with a readable error — the caller
// surfaces it rather than silently dropping the image. ctx bounds the downloads.
//
// Empty-block filtering (Tech Design §2.1 — fixes the Turn-2 cascade): AWS Bedrock
// Converse REJECTS a content block that sets none of its members and rejects a
// message with an empty content array ("The ContentBlock object at messages.N.
// content.0 must set one of ..."). An empty assistant turn from a previous round
// (e.g. an upstream that produced no text) would otherwise serialize to {text:""}
// and break the next request. So we emit a text block ONLY when c.Text != "",
// emit image blocks as-is, and SKIP any message whose filtered block list is
// empty (never send a content-less message / empty content array upstream). This
// is Bedrock-only — OpenAI accepts empty-string content and is unaffected. A
// non-empty text-only message still serializes byte-identically to before.
func unifiedToBedrock(ctx context.Context, uni UnifiedRequest) (bedrockConverseRequest, error) {
	out := bedrockConverseRequest{}
	if strings.TrimSpace(uni.System) != "" {
		out.System = []bedrockSystemBlock{{Text: uni.System}}
	}
	for _, m := range uni.Messages {
		blocks := make([]bedrockContentBlock, 0, len(m.Content))
		for _, c := range m.Content {
			if c.IsImage() {
				img, err := bedrockImageFromSource(ctx, c.Image)
				if err != nil {
					return bedrockConverseRequest{}, err
				}
				blocks = append(blocks, bedrockContentBlock{Image: img})
				continue
			}
			// Text block: skip empties. Bedrock rejects {text:""}, and an empty
			// text block carries no information anyway.
			if c.Text == "" {
				continue
			}
			blocks = append(blocks, bedrockContentBlock{Text: c.Text})
		}
		// A message with no usable blocks (e.g. an empty assistant placeholder)
		// must not be sent — Bedrock rejects an empty content array.
		if len(blocks) == 0 {
			continue
		}
		out.Messages = append(out.Messages, bedrockMessage{Role: m.Role, Content: blocks})
	}
	if uni.MaxTokens != nil || uni.Temperature != nil || uni.TopP != nil || len(uni.StopSequences) > 0 {
		out.InferenceConfig = &bedrockInferenceConfig{
			MaxTokens:     uni.MaxTokens,
			Temperature:   uni.Temperature,
			TopP:          uni.TopP,
			StopSequences: uni.StopSequences,
		}
	}
	return out, nil
}

// bedrockImageFromSource materializes a unified image block into a Converse image
// block. base64 sources are used directly; url sources are downloaded to base64
// (the only way Converse can ingest a remote image).
//
// The Converse format token is determined by SNIFFING the actual image bytes,
// not by trusting the declared media type / URL extension. Converse validates
// the bytes against the declared format, so a wrong or unsupported label (HEIC,
// AVIF, BMP, SVG, or a browser mislabel) produces the opaque upstream error
// "Could not process image". When the bytes are one of Bedrock's four supported
// formats we send the sniffed format; otherwise we fail fast with a readable
// error naming the unsupported type, instead of mislabeling it as png.
func bedrockImageFromSource(ctx context.Context, src *ImageSource) (*bedrockImageBlock, error) {
	var b64, mediaType string
	switch src.Kind {
	case ImageKindBase64:
		b64, mediaType = src.Data, src.MediaType
	case ImageKindURL:
		mt, data, err := DownloadImageToBase64(ctx, src.URL)
		if err != nil {
			return nil, fmt.Errorf("bedrock image: %w", err)
		}
		b64, mediaType = data, mt
	default:
		return nil, fmt.Errorf("bedrock image: unknown image source kind %q", src.Kind)
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("bedrock image: decode base64 (declared %s): %w", imageFormatLabel(mediaType), err)
	}
	// Prefer the sniffed format (source of truth); fall back to the declared
	// media type only when sniffing is inconclusive (e.g. a format we don't
	// recognize but Bedrock might still take per the label).
	format := sniffImageFormat(raw)
	if format == "" {
		format = bedrockImageFormat(mediaType)
	}
	if format == "" {
		return nil, fmt.Errorf(
			"bedrock image: unsupported image format (declared %s); Bedrock Converse accepts only PNG, JPEG, GIF, or WebP — convert the image and retry",
			imageFormatLabel(mediaType),
		)
	}
	return &bedrockImageBlock{
		Format: format,
		Source: bedrockImageSource{Bytes: b64},
	}, nil
}

// bedrockToUnifiedResponse converts a Bedrock Converse response into a
// UnifiedResponse (Usage is attached by the caller after fallback handling).
func bedrockToUnifiedResponse(r bedrockConverseResponse) UnifiedResponse {
	out := UnifiedResponse{StopReason: bedrockStopToUnified(r.StopReason)}
	for _, c := range r.Output.Message.Content {
		if c.Text != "" {
			out.Content = append(out.Content, TextBlock(c.Text))
		}
	}
	return out
}

// ========================= Stop-reason mapping ============================

// openAIFinishToStop maps an OpenAI finish_reason to the normalized StopReason.
func openAIFinishToStop(s string) StopReason {
	switch s {
	case "stop":
		return StopEndTurn
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopContentFilter
	case "":
		return StopUnknown
	default:
		return StopEndTurn
	}
}

// stopToOpenAIFinish maps a normalized StopReason back to an OpenAI finish_reason.
func stopToOpenAIFinish(s StopReason) string {
	switch s {
	case StopEndTurn:
		return "stop"
	case StopStopSequence:
		return "stop"
	case StopMaxTokens:
		return "length"
	case StopToolUse:
		return "tool_calls"
	case StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// anthropicStopToUnified maps an Anthropic stop_reason to the normalized value.
func anthropicStopToUnified(s string) StopReason {
	switch s {
	case "end_turn":
		return StopEndTurn
	case "max_tokens":
		return StopMaxTokens
	case "stop_sequence":
		return StopStopSequence
	case "tool_use":
		return StopToolUse
	case "":
		return StopUnknown
	default:
		return StopEndTurn
	}
}

// stopToAnthropic maps a normalized StopReason back to an Anthropic stop_reason.
func stopToAnthropic(s StopReason) string {
	switch s {
	case StopEndTurn:
		return "end_turn"
	case StopMaxTokens:
		return "max_tokens"
	case StopStopSequence:
		return "stop_sequence"
	case StopToolUse:
		return "tool_use"
	case StopContentFilter:
		// Anthropic has no direct content_filter stop; end_turn is the closest.
		return "end_turn"
	default:
		return "end_turn"
	}
}

// bedrockStopToUnified maps a Bedrock Converse stopReason to the normalized value.
func bedrockStopToUnified(s string) StopReason {
	switch s {
	case "end_turn":
		return StopEndTurn
	case "max_tokens":
		return StopMaxTokens
	case "stop_sequence":
		return StopStopSequence
	case "tool_use":
		return StopToolUse
	case "content_filtered", "guardrail_intervened":
		return StopContentFilter
	case "":
		return StopUnknown
	default:
		return StopEndTurn
	}
}

// stopToBedrock maps a normalized StopReason to a Bedrock Converse stopReason.
// Provided for completeness / symmetry (the relay only sends requests, but tests
// and any reverse mapping rely on it).
func stopToBedrock(s StopReason) string {
	switch s {
	case StopEndTurn:
		return "end_turn"
	case StopMaxTokens:
		return "max_tokens"
	case StopStopSequence:
		return "stop_sequence"
	case StopToolUse:
		return "tool_use"
	case StopContentFilter:
		return "content_filtered"
	default:
		return "end_turn"
	}
}

// describeRequest is a small debug helper (kept un-exported) used in tests to
// render a unified request compactly.
func describeRequest(uni UnifiedRequest) string {
	return fmt.Sprintf("model=%s system=%q msgs=%d stream=%v", uni.Model, uni.System, len(uni.Messages), uni.Stream)
}
