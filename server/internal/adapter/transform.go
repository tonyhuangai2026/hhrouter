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
	// Tools is the OpenAI tool list ([{type:"function",function:{name,description,
	// parameters}}]); ToolChoice the OpenAI tool_choice directive. Both are parsed
	// into the canonical (Anthropic-shaped) unified representation by
	// ParseOpenAIRequest so any upstream adapter can re-render them.
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// OpenAIInboundMessage allows content to be a string or a parts array. tool_calls
// (on an assistant turn) and tool_call_id (on a role=tool turn) carry the OpenAI
// function-calling fields.
type OpenAIInboundMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall is one element of an assistant message's tool_calls array.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
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
	uni.Tools = openAIToolsToCanonical(in.Tools)
	uni.ToolChoice = openAIToolChoiceToCanonical(in.ToolChoice)

	var systems []string
	for _, m := range in.Messages {
		if m.Role == RoleSystem {
			// System is text-only; flatten any parts to text.
			if text := openAIContentToText(m.Content); text != "" {
				systems = append(systems, text)
			}
			continue
		}
		if m.Role == "tool" {
			// An OpenAI tool result turn. OpenAI carries these as standalone
			// role=tool messages; the canonical (Anthropic) model nests a
			// tool_result block inside a user turn.
			content := openAIToolResultContent(m.Content)
			uni.Messages = append(uni.Messages, Message{
				Role:    RoleUser,
				Content: []ContentBlock{ToolResultBlockOf(m.ToolCallID, content, false)},
			})
			continue
		}
		blocks := openAIContentToBlocks(m.Content)
		// Assistant tool_calls → tool_use blocks (appended after any text).
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, ToolUseBlockOf(tc.ID, tc.Function.Name, openAIArgsToInput(tc.Function.Arguments)))
		}
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

// openAIArgsToInput converts an OpenAI tool_call function.arguments value (which
// the API encodes as a JSON *string* containing JSON) into a canonical tool_use
// input (a JSON *object*). If args is already a JSON object/value (some clients
// send it unquoted) it is passed through; an empty/blank value becomes {}.
func openAIArgsToInput(args json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(args))
	if s == "" || s == "null" || s == `""` {
		return json.RawMessage(`{}`)
	}
	// If it's a JSON string, unquote it to recover the inner JSON object.
	var inner string
	if err := json.Unmarshal(args, &inner); err == nil {
		if strings.TrimSpace(inner) == "" {
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(inner)
	}
	// Already a JSON object/array/value.
	return args
}

// openAIToolResultContent normalizes an OpenAI tool message's content (a string,
// or occasionally a parts array) into the canonical tool_result content (raw
// JSON). A plain string is kept as a JSON string.
func openAIToolResultContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`""`)
	}
	return raw
}

// openAIToolsToCanonical converts an OpenAI tools array
// ([{type:"function",function:{name,description,parameters}}]) into the canonical
// Anthropic-shaped tools array ([{name,description,input_schema}]). Returns nil
// when there are no tools.
func openAIToolsToCanonical(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var tools []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(strings.TrimSpace(string(schema))) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, map[string]any{
			"name":         t.Function.Name,
			"description":  t.Function.Description,
			"input_schema": schema,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// openAIToolChoiceToCanonical maps an OpenAI tool_choice to the Anthropic
// tool_choice shape. "auto"/"none"/"required" strings map to {type:...}; an
// object {type:"function",function:{name}} maps to {type:"tool",name}. Unknown
// shapes return nil (omit).
func openAIToolChoiceToCanonical(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch str {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		default:
			return nil
		}
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		b, _ := json.Marshal(map[string]any{"type": "tool", "name": obj.Function.Name})
		return b
	}
	return nil
}

// addCacheUsageAnthropic renders the prompt-cache token counts into an Anthropic
// usage map, matching the native Anthropic field names. Only non-zero counts are
// added so a request without caching produces a byte-identical usage object.
func addCacheUsageAnthropic(m map[string]any, u Usage) {
	if m == nil {
		return
	}
	if u.CacheReadTokens > 0 {
		m["cache_read_input_tokens"] = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		m["cache_creation_input_tokens"] = u.CacheWriteTokens
	}
}

// addCacheUsageOpenAI renders the prompt-cache read count into an OpenAI usage map
// under prompt_tokens_details.cached_tokens. OpenAI reports no separate cache-write
// count, so only the read bucket is mapped, and only when non-zero (kept absent
// otherwise for backward compatibility).
func addCacheUsageOpenAI(m map[string]any, u Usage) {
	if m == nil {
		return
	}
	if u.CacheReadTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
}

// addCacheUsageBedrock renders the prompt-cache token counts into a Bedrock
// Converse usage map, matching the native cacheReadInputTokens/cacheWriteInputTokens
// field names. Only non-zero counts are added.
func addCacheUsageBedrock(m map[string]any, u Usage) {
	if m == nil {
		return
	}
	if u.CacheReadTokens > 0 {
		m["cacheReadInputTokens"] = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		m["cacheWriteInputTokens"] = u.CacheWriteTokens
	}
}

// AnthropicStreamDeltaUsage builds the usage object for the terminal Anthropic
// streaming message_delta. output_tokens is always present; input_tokens and the
// cache buckets are added only when non-zero, so a no-cache stream keeps the
// existing {output_tokens} shape. Used by the relay's hand-built terminal
// message_delta (the Anthropic streaming output path bypasses emitChunk).
func AnthropicStreamDeltaUsage(u Usage) map[string]any {
	m := map[string]any{"output_tokens": u.CompletionTokens}
	if u.PromptTokens > 0 {
		m["input_tokens"] = u.PromptTokens
	}
	addCacheUsageAnthropic(m, u)
	return m
}

// BuildOpenAIResponse converts a UnifiedResponse into an inbound OpenAI Chat
// Completions response body (id/created are filled by the relay).
func BuildOpenAIResponse(r UnifiedResponse) map[string]any {
	message := map[string]any{"role": RoleAssistant, "content": r.Text()}
	// Surface any tool_use blocks as OpenAI tool_calls so a tool-using upstream
	// response reaches an OpenAI client intact (not flattened to empty text).
	var toolCalls []map[string]any
	for _, c := range r.Content {
		if !c.IsToolUse() {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   c.ToolUse.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.ToolUse.Name,
				"arguments": inputToOpenAIArgs(c.ToolUse.Input),
			},
		})
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		// OpenAI sends content:null when the turn is purely tool calls.
		if r.Text() == "" {
			message["content"] = nil
		}
	}
	usage := map[string]any{
		"prompt_tokens":     r.Usage.PromptTokens,
		"completion_tokens": r.Usage.CompletionTokens,
		"total_tokens":      r.Usage.TotalTokens,
	}
	addCacheUsageOpenAI(usage, r.Usage)
	return map[string]any{
		"object": "chat.completion",
		"model":  r.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": stopToOpenAIFinish(r.StopReason),
			},
		},
		"usage": usage,
	}
}

// BuildOpenAIStreamChunk converts a unified StreamChunk into an inbound OpenAI
// SSE chunk object (the value that follows "data: "). Returns nil for the [DONE]
// sentinel so the caller can emit the literal "data: [DONE]" instead.
func BuildOpenAIStreamChunk(model string, c StreamChunk) map[string]any {
	if c.Done && c.Delta == "" && c.StopReason == StopUnknown && c.Usage == nil && c.ToolCallDelta == nil {
		return nil
	}

	usageObj := func() map[string]any {
		if c.Usage == nil {
			return nil
		}
		m := map[string]any{
			"prompt_tokens":     c.Usage.PromptTokens,
			"completion_tokens": c.Usage.CompletionTokens,
			"total_tokens":      c.Usage.TotalTokens,
		}
		addCacheUsageOpenAI(m, *c.Usage)
		return m
	}

	// A chunk that carries ONLY usage (no content delta, no tool call, no stop
	// reason) must use an EMPTY choices array — the OpenAI streaming contract for
	// the include_usage chunk. This is exactly what an Anthropic message_start
	// (prompt tokens, no content) maps to. Emitting choices:[{delta:{}}] makes
	// strict clients (opencode) fail union validation ("No matching discriminator")
	// because an empty delta matches none of their chunk variants.
	hasContent := c.Delta != "" || c.ToolCallDelta != nil || c.StopReason != StopUnknown
	if !hasContent {
		out := map[string]any{
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{},
		}
		if u := usageObj(); u != nil {
			out["usage"] = u
		}
		return out
	}

	delta := map[string]any{}
	if c.Delta != "" {
		delta["content"] = c.Delta
	}
	if tc := c.ToolCallDelta; tc != nil {
		fn := map[string]any{}
		if tc.Name != "" {
			fn["name"] = tc.Name
		}
		// arguments is always present (possibly "") on a tool_calls delta.
		fn["arguments"] = tc.ArgsFragment
		call := map[string]any{"index": tc.Index, "type": "function", "function": fn}
		if tc.ID != "" {
			call["id"] = tc.ID
		}
		delta["tool_calls"] = []map[string]any{call}
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
	if u := usageObj(); u != nil {
		out["usage"] = u
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
	// Tools / ToolChoice are kept as raw JSON and forwarded verbatim so the full
	// tool definitions (name, description, input_schema) survive the gateway.
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
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
	// tool_use block fields (assistant requesting a tool call).
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result block fields (user returning a tool's output).
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	// cache_control marks a prompt-cache breakpoint after this block. Only the
	// presence (and type) matters; kept as a minimal struct so it survives the
	// string↔block-array decode without pulling in the whole shape.
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control,omitempty"`
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
		before := len(out)
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
		case "tool_use":
			// An assistant tool-call request. Keep id/name/input verbatim.
			out = append(out, ToolUseBlockOf(b.ID, b.Name, b.Input))
		case "tool_result":
			// A user-supplied tool result referencing a prior tool_use by id.
			out = append(out, ToolResultBlockOf(b.ToolUseID, b.Content, b.IsError))
		case "text", "":
			if b.Text != "" {
				out = append(out, TextBlock(b.Text))
			}
		default:
			if b.Text != "" {
				out = append(out, TextBlock(b.Text))
			}
		}
		// Carry a cache_control breakpoint onto the block we just emitted (a
		// breakpoint applies AFTER its anchor block). If this inbound block produced
		// no unified block (e.g. an empty/unusable block) there is no anchor to hang
		// it on, so it is dropped.
		if b.CacheControl != nil && len(out) > before {
			out[len(out)-1].CacheControl = &CacheControl{Type: b.CacheControl.Type}
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

// anthropicSystemHasCacheControl reports whether an Anthropic system field, in its
// block-array form, has any block carrying cache_control — the signal that the
// whole system prompt should end with a cache breakpoint. A plain-string system
// carries no cache_control (returns false), matching the Anthropic contract.
func anthropicSystemHasCacheControl(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return false
	}
	var blocks []struct {
		CacheControl *struct {
			Type string `json:"type"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.CacheControl != nil {
			return true
		}
	}
	return false
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
		Tools:         in.Tools,
		ToolChoice:    in.ToolChoice,
	}
	// Anthropic's contract carries system in the top-level `system` field, but some
	// clients (e.g. Claude Code) also slip a role="system" entry into messages.
	// Upstreams reject any messages[].role that is not user/assistant, so hoist
	// those out into System (mirroring the OpenAI inbound path) rather than
	// forwarding them verbatim.
	var systems []string
	systemCached := anthropicSystemHasCacheControl(in.System)
	if s := strings.TrimSpace(uni.System); s != "" {
		systems = append(systems, s)
	}
	for _, m := range in.Messages {
		if m.Role == RoleSystem {
			// System is text-only; reuse the string/block-array flattener.
			if text := strings.TrimSpace(anthropicSystemToText(m.Content)); text != "" {
				systems = append(systems, text)
			}
			// A role=system message in block-array form may also carry a cache
			// breakpoint; fold it into the single system-level breakpoint.
			if anthropicSystemHasCacheControl(m.Content) {
				systemCached = true
			}
			continue
		}
		uni.Messages = append(uni.Messages, Message{Role: m.Role, Content: anthropicContentToBlocks(m.Content)})
	}
	uni.System = strings.Join(systems, "\n")
	if systemCached {
		uni.SystemCacheControl = &CacheControl{Type: "ephemeral"}
	}
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
	b, ok := buildAnthropicContentBlock(c)
	if ok && c.CacheControl != nil {
		// A breakpoint on this block round-trips as cache_control:{type:"ephemeral"}
		// so an Anthropic upstream re-establishes the same cache point.
		b["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return b, ok
}

// buildAnthropicContentBlock is the cache-control-agnostic core of
// BuildAnthropicContentBlock (kept separate so cache_control is attached once,
// after the block type has been resolved).
func buildAnthropicContentBlock(c ContentBlock) (block map[string]any, ok bool) {
	if c.IsToolUse() {
		// Anthropic requires input to be a JSON object; default to {} when absent
		// so the upstream does not reject a null/empty input.
		input := json.RawMessage(c.ToolUse.Input)
		if strings.TrimSpace(string(input)) == "" {
			input = json.RawMessage(`{}`)
		}
		return map[string]any{
			"type":  "tool_use",
			"id":    c.ToolUse.ID,
			"name":  c.ToolUse.Name,
			"input": input,
		}, true
	}
	if c.IsToolResult() {
		b := map[string]any{
			"type":        "tool_result",
			"tool_use_id": c.ToolResult.ToolUseID,
		}
		if strings.TrimSpace(string(c.ToolResult.Content)) != "" {
			b["content"] = json.RawMessage(c.ToolResult.Content)
		}
		if c.ToolResult.IsError {
			b["is_error"] = true
		}
		return b, true
	}
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
		// Reuse the block builder so tool_use blocks (and images) round-trip,
		// not just text.
		if block, ok := BuildAnthropicContentBlock(c); ok {
			content = append(content, block)
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	usage := map[string]any{
		"input_tokens":  r.Usage.PromptTokens,
		"output_tokens": r.Usage.CompletionTokens,
	}
	addCacheUsageAnthropic(usage, r.Usage)
	return map[string]any{
		"type":        "message",
		"role":        RoleAssistant,
		"model":       r.Model,
		"content":     content,
		"stop_reason": stopToAnthropic(r.StopReason),
		"usage":       usage,
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
	content := make([]map[string]any, 0, len(r.Content)+1)
	// Preserve the existing contract of a single joined text block (r.Text()
	// concatenates all text blocks), then append any toolUse blocks.
	if text := r.Text(); text != "" {
		content = append(content, map[string]any{"text": text})
	}
	for _, c := range r.Content {
		if !c.IsToolUse() {
			continue
		}
		input := json.RawMessage(c.ToolUse.Input)
		if strings.TrimSpace(string(input)) == "" {
			input = json.RawMessage(`{}`)
		}
		content = append(content, map[string]any{
			"toolUse": map[string]any{
				"toolUseId": c.ToolUse.ID,
				"name":      c.ToolUse.Name,
				"input":     input,
			},
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"text": ""})
	}
	usage := map[string]any{
		"inputTokens":  r.Usage.PromptTokens,
		"outputTokens": r.Usage.CompletionTokens,
		"totalTokens":  r.Usage.TotalTokens,
	}
	addCacheUsageBedrock(usage, r.Usage)
	return map[string]any{
		"output": map[string]any{
			"message": map[string]any{
				"role":    RoleAssistant,
				"content": content,
			},
		},
		"stopReason": stopToBedrock(r.StopReason),
		"usage":      usage,
	}
}

// Bedrock ConverseStream event-type discriminators (the :event-type header
// value the relay stamps via EncodeBedrockFrame).
const (
	BedrockEventMessageStart      = "messageStart"
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
	usage := map[string]any{
		"inputTokens":  u.PromptTokens,
		"outputTokens": u.CompletionTokens,
		"totalTokens":  u.TotalTokens,
	}
	addCacheUsageBedrock(usage, u)
	return map[string]any{"usage": usage}
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
		// A system-level cache breakpoint becomes a trailing {cachePoint} system
		// block (Bedrock caches everything up to it). Only append when there is
		// actual system text to anchor it — never produce an orphan cachePoint.
		if uni.SystemCacheControl != nil {
			out.System = append(out.System, bedrockSystemBlock{CachePoint: &bedrockCachePoint{Type: "default"}})
		}
	}
	for _, m := range uni.Messages {
		blocks := make([]bedrockContentBlock, 0, len(m.Content))
		for _, c := range m.Content {
			// Track whether this source block produced a real (non-cachePoint)
			// content block, so a trailing cachePoint is only appended after a real
			// anchor and the empty-message filter still counts real content.
			before := len(blocks)
			if c.IsImage() {
				img, err := bedrockImageFromSource(ctx, c.Image)
				if err != nil {
					return bedrockConverseRequest{}, err
				}
				blocks = append(blocks, bedrockContentBlock{Image: img})
			} else if c.IsToolUse() {
				input := json.RawMessage(c.ToolUse.Input)
				if strings.TrimSpace(string(input)) == "" {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, bedrockContentBlock{ToolUse: &bedrockToolUseBlock{
					ToolUseID: c.ToolUse.ID, Name: c.ToolUse.Name, Input: input,
				}})
			} else if c.IsToolResult() {
				trContent, err := bedrockToolResultContent(ctx, c.ToolResult.Content)
				if err != nil {
					return bedrockConverseRequest{}, err
				}
				blocks = append(blocks, bedrockContentBlock{ToolResult: &bedrockToolResultBlock{
					ToolUseID: c.ToolResult.ToolUseID,
					Content:   trContent,
				}})
			} else if c.Text != "" {
				// Text block: skip empties. Bedrock rejects {text:""}, and an empty
				// text block carries no information anyway.
				blocks = append(blocks, bedrockContentBlock{Text: c.Text})
			}
			// A cache breakpoint on this source block becomes a {cachePoint} content
			// block placed right AFTER its anchor — but only if the anchor actually
			// rendered a real block (an empty text block produces no anchor, so its
			// cachePoint would be orphaned and is dropped).
			if c.CacheControl != nil && len(blocks) > before {
				blocks = append(blocks, bedrockContentBlock{CachePoint: &bedrockCachePoint{Type: "default"}})
			}
		}
		// A message with no usable blocks (e.g. an empty assistant placeholder)
		// must not be sent — Bedrock rejects an empty content array. A cachePoint is
		// only ever appended after a real anchor block above, so blocks is never a
		// lone cachePoint here.
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
	out.ToolConfig = canonicalToolsToBedrock(uni.Tools, uni.ToolChoice)
	// Bedrock requires toolConfig to be defined whenever the conversation contains
	// any toolUse/toolResult content block — even on a follow-up request that no
	// longer sends the `tools` list (e.g. Claude Code's Stop-hook call reusing a
	// history that has tool blocks). Without it Bedrock rejects with
	// TOOL_CONFIG_MISSING. When the request itself declared no tools but the
	// history references some, synthesize a minimal toolConfig from the tool_use
	// blocks' names so the request is accepted; this does not change model behavior
	// on a turn that isn't asking for a new tool call.
	if out.ToolConfig == nil {
		if cfg := reconstructToolConfigFromHistory(uni.Messages); cfg != nil {
			out.ToolConfig = cfg
		}
	}
	return out, nil
}

// reconstructToolConfigFromHistory builds a minimal Bedrock toolConfig from the
// tool_use blocks present in a conversation, used only as a fallback when the
// request carried no `tools` field but its history contains tool blocks (Bedrock
// then demands a toolConfig). Each distinct tool name becomes a toolSpec with a
// permissive object schema. Returns nil if there are no tool_use blocks.
func reconstructToolConfigFromHistory(messages []Message) *bedrockToolConfig {
	seen := map[string]bool{}
	var cfg *bedrockToolConfig
	for _, m := range messages {
		for _, c := range m.Content {
			if !c.IsToolUse() || c.ToolUse.Name == "" || seen[c.ToolUse.Name] {
				continue
			}
			seen[c.ToolUse.Name] = true
			if cfg == nil {
				cfg = &bedrockToolConfig{}
			}
			cfg.Tools = append(cfg.Tools, bedrockTool{ToolSpec: bedrockToolSpec{
				Name:        c.ToolUse.Name,
				InputSchema: map[string]any{"json": map[string]any{"type": "object"}},
			}})
		}
	}
	return cfg
}

// toolsEffectivelyEmpty reports whether a raw canonical `tools` value carries no
// usable tool definitions. This is true for an absent field, whitespace, JSON
// null, AND an empty array `[]` — the last one matters because Claude Code's
// "goal" mode sends `"tools": []` (rather than omitting the field) on follow-up
// turns. An empty array is NOT the same as "tools present": forwarding `tools:[]`
// alongside tool_use/tool_result history still makes Bedrock reject the request
// with TOOL_CONFIG_MISSING, so callers must treat it as empty and reconstruct.
func toolsEffectivelyEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return true
	}
	// Cheap check for an empty array in any whitespace form, e.g. "[]" or "[ ]".
	if s[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) == 0 {
			return true
		}
	}
	return false
}

// ReconstructCanonicalToolsFromHistory builds a canonical (Anthropic-shaped)
// tools array — [{"name":..,"description":..,"input_schema":{"type":"object"}}] —
// from the distinct tool_use blocks in a conversation. It is the format-agnostic
// analogue of reconstructToolConfigFromHistory: used when a request carries no
// `tools` field but its history references tool calls, and the upstream (Bedrock,
// or an Anthropic-compatible gateway fronting Bedrock) rejects such a request with
// TOOL_CONFIG_MISSING. Returns nil (not an empty array) when there are no tool_use
// blocks, so callers can leave `tools` absent for ordinary conversations.
func ReconstructCanonicalToolsFromHistory(messages []Message) json.RawMessage {
	type canonicalTool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	seen := map[string]bool{}
	var tools []canonicalTool
	for _, m := range messages {
		for _, c := range m.Content {
			if !c.IsToolUse() || c.ToolUse.Name == "" || seen[c.ToolUse.Name] {
				continue
			}
			seen[c.ToolUse.Name] = true
			tools = append(tools, canonicalTool{
				Name:        c.ToolUse.Name,
				InputSchema: map[string]any{"type": "object"},
			})
		}
	}
	if len(tools) == 0 {
		return nil
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil
	}
	return raw
}

// bedrockToolResultContent renders a canonical tool_result content (raw JSON)
// into Bedrock's toolResult.content list. Converse is strict about each sub-block:
// a {json:…} sub-block's value MUST be a JSON OBJECT (not an array/string/number),
// and a {text:…} sub-block's value must be a string. Getting this wrong yields the
// 400 "The format of the value at messages.N.content.M.toolResult.content.K.json
// is invalid. Provide a json object for the field."
//
// The canonical (Anthropic-shaped) tool_result content can be any of:
//   - a plain string                       → one {text} sub-block
//   - a JSON object                        → one {json} sub-block (object is valid)
//   - an Anthropic content-block ARRAY,
//     e.g. [{"type":"text","text":"..."}]  → one sub-block PER element, unpacked
//     to {text}/{image}/{json} by block type (this is the common Anthropic shape,
//     and the source of the reported bug: an array must NOT be dumped under json)
//   - a bare JSON array of non-blocks       → {json} per element that is an object,
//     else {text} of that element's JSON
//
// ctx bounds any image downloads (url image blocks inside a tool result).
func bedrockToolResultContent(ctx context.Context, raw json.RawMessage) ([]map[string]any, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return []map[string]any{{"text": ""}}, nil
	}
	// Plain string → text.
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return []map[string]any{{"text": str}}, nil
	}
	// Array: treat each element as an Anthropic content block and unpack.
	if s[0] == '[' {
		var blocks []anthropicInboundBlock
		if err := json.Unmarshal(raw, &blocks); err == nil {
			out := make([]map[string]any, 0, len(blocks))
			for _, b := range blocks {
				switch b.Type {
				case "text", "":
					// A well-formed text block, or a typeless {"text":...}.
					if b.Text != "" || b.Type == "text" {
						out = append(out, map[string]any{"text": b.Text})
					}
				case "image":
					if b.Source == nil {
						continue
					}
					var cb ContentBlock
					switch b.Source.Type {
					case "url":
						cb = ImageURLBlock(b.Source.URL)
					default:
						cb = ImageBase64Block(b.Source.MediaType, b.Source.Data)
					}
					img, err := bedrockImageFromSource(ctx, cb.Image)
					if err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"image": img})
				default:
					// Unknown block type: fall back to embedding the raw element.
					out = append(out, map[string]any{"text": b.Text})
				}
			}
			if len(out) == 0 {
				return []map[string]any{{"text": ""}}, nil
			}
			return out, nil
		}
		// Not an array of content blocks — wrap the whole array's items. Bedrock
		// json must be an object, so a bare array can't go under json; stringify.
		return []map[string]any{{"text": s}}, nil
	}
	// JSON object → json sub-block (valid: json requires an object).
	if s[0] == '{' {
		return []map[string]any{{"json": json.RawMessage(raw)}}, nil
	}
	// Any other scalar (number/bool/null) → text of its literal.
	return []map[string]any{{"text": s}}, nil
}

// canonicalToolsToBedrock converts the canonical (Anthropic-shaped) tools array
// and tool_choice into a Bedrock toolConfig. Returns nil when there are no tools.
func canonicalToolsToBedrock(toolsRaw, choiceRaw json.RawMessage) *bedrockToolConfig {
	if toolsEffectivelyEmpty(toolsRaw) {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return nil
	}
	cfg := &bedrockToolConfig{}
	for _, t := range tools {
		var schema map[string]any
		if len(strings.TrimSpace(string(t.InputSchema))) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema)
		}
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		cfg.Tools = append(cfg.Tools, bedrockTool{ToolSpec: bedrockToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: map[string]any{"json": schema},
		}})
	}
	cfg.ToolChoice = canonicalToolChoiceToBedrock(choiceRaw)
	return cfg
}

// canonicalToolChoiceToBedrock maps the canonical tool_choice to Bedrock's
// toolChoice ({auto:{}} | {any:{}} | {tool:{name}}). {type:none} has no Bedrock
// equivalent (Bedrock has no "none") → nil (omit). nil/unknown → nil.
func canonicalToolChoiceToBedrock(raw json.RawMessage) map[string]any {
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
		return map[string]any{"auto": map[string]any{}}
	case "any":
		return map[string]any{"any": map[string]any{}}
	case "tool":
		if c.Name != "" {
			return map[string]any{"tool": map[string]any{"name": c.Name}}
		}
	}
	return nil
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
		switch {
		case c.ToolUse != nil:
			out.Content = append(out.Content, ToolUseBlockOf(c.ToolUse.ToolUseID, c.ToolUse.Name, c.ToolUse.Input))
		case c.Text != "":
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

// StopToAnthropicWire is the exported form of stopToAnthropic, used by the relay
// when it renders Anthropic stream framing directly (tool-call block handling).
func StopToAnthropicWire(s StopReason) string { return stopToAnthropic(s) }

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
