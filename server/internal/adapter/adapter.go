// Package adapter is the upstream adapter layer (Tech Design §6). It defines a
// provider-agnostic intermediate representation (UnifiedRequest / UnifiedResponse
// / Usage) plus an Adapter interface that turns a unified request into a concrete
// upstream HTTP request and parses the upstream response (and stream chunks) back
// into the unified representation.
//
// This package is a PURE LIBRARY: it builds *http.Request values and parses
// *http.Response / raw chunk bytes, but never registers HTTP routes and never
// performs the actual round-trip. The relay layer (T7) owns the http.Client call,
// SSE streaming, quota accounting and request logging; it wires these adapters in.
//
// Two adapters are provided:
//   - OpenAIAdapter   → forwards to {base_url}/v1/chat/completions (Bearer key).
//   - BedrockAdapter  → POSTs to the Bedrock Runtime Converse / ConverseStream
//     endpoint using an AWS Bedrock bearer API key (no SigV4). See
//     bedrock_adapter.go for the endpoint/auth verification notes.
//
// The transform.go file converts between the two inbound wire formats this
// platform exposes (OpenAI Chat Completions and Anthropic Messages) and the
// unified representation, including streaming chunk conversion. usage.go parses
// token usage out of upstream responses with a char-based estimate fallback.
package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agent-router/server/internal/model"
)

// Role values used across the unified representation. Inbound OpenAI/Anthropic
// roles and the upstream Bedrock roles all normalize to these.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// StopReason is a normalized finish reason. Each adapter/transform maps its
// provider-specific value into one of these so the relay can re-emit the right
// inbound-format value regardless of which upstream served the request.
type StopReason string

const (
	// StopEndTurn: the model finished naturally (OpenAI "stop",
	// Anthropic "end_turn", Bedrock "end_turn").
	StopEndTurn StopReason = "end_turn"
	// StopMaxTokens: output truncated by the max-tokens limit (OpenAI "length",
	// Anthropic "max_tokens", Bedrock "max_tokens").
	StopMaxTokens StopReason = "max_tokens"
	// StopStopSequence: a stop sequence was hit (OpenAI "stop" with a matched
	// sequence, Anthropic "stop_sequence", Bedrock "stop_sequence").
	StopStopSequence StopReason = "stop_sequence"
	// StopToolUse: the model wants to call a tool (OpenAI "tool_calls",
	// Anthropic "tool_use", Bedrock "tool_use").
	StopToolUse StopReason = "tool_use"
	// StopContentFilter: blocked by a content filter / guardrail.
	StopContentFilter StopReason = "content_filter"
	// StopUnknown: provider returned no / an unrecognized finish reason.
	StopUnknown StopReason = ""
)

// Block type constants for ContentBlock.Type. Text is the original/default kind;
// Image was added to carry multimodal image parts through the unified layer;
// ToolUse / ToolResult carry tool-calling turns (assistant requests a tool,
// user returns its result) so agent clients (Claude Code, Cursor, …) work.
const (
	BlockText       = "text"
	BlockImage      = "image"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// ImageKind enumerates how an image's bytes are referenced in the unified layer.
const (
	// ImageKindURL: the image is referenced by a remote http(s) URL. The bytes
	// are NOT inlined; an upstream that only accepts inline bytes (Bedrock) must
	// download the URL first (see DownloadImageToBase64).
	ImageKindURL = "url"
	// ImageKindBase64: the image bytes are inlined as base64 (no data: prefix),
	// with MediaType set to the IANA media type (e.g. "image/png").
	ImageKindBase64 = "base64"
)

// ContentBlock is one piece of a message's content. A block is either text
// (Type=="text", Text holds the payload) or an image (Type=="image", Image holds
// the source). The Type field leaves room for further block kinds (tool_use, …)
// without breaking the shape.
//
// Backward compatibility: existing callers construct text blocks via TextBlock
// and read Text directly; image blocks leave Text empty, so the text-flattening
// helpers (Message.Text / UnifiedResponse.Text) and any text-only upstream path
// behave exactly as before — image blocks contribute no text.
type ContentBlock struct {
	Type  string       `json:"type"`            // "text" | "image" | "tool_use" | "tool_result"
	Text  string       `json:"text,omitempty"`  // when Type=="text"
	Image *ImageSource `json:"image,omitempty"` // when Type=="image"

	// ToolUse is set when Type=="tool_use": an assistant turn asking to invoke a
	// tool. ToolResult is set when Type=="tool_result": a user turn returning a
	// tool's output, referenced back by ToolUseID. Both are nil otherwise so the
	// text-flattening helpers and text-only upstream paths are unaffected.
	ToolUse    *ToolUseBlock    `json:"tool_use,omitempty"`
	ToolResult *ToolResultBlock `json:"tool_result,omitempty"`
}

// ToolUseBlock is a model's request to call a tool. ID is the provider-assigned
// call id (echoed back by the matching tool_result); Name is the tool name;
// Input is the raw JSON arguments object (kept verbatim so no precision is lost
// round-tripping through the gateway).
type ToolUseBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultBlock is the result of a tool call, supplied by the client on the
// next turn. ToolUseID references the originating ToolUseBlock.ID. Content is the
// raw JSON of the result (a string or an array of blocks, per the Anthropic
// contract) kept verbatim. IsError marks a tool-execution error.
type ToolResultBlock struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ImageSource describes the bytes of an image content block. Exactly one of the
// two reference kinds applies:
//   - Kind=="url":    URL holds an http(s) URL; MediaType/Data are empty.
//   - Kind=="base64": Data holds base64-encoded bytes (NO "data:" prefix) and
//     MediaType holds the IANA media type (e.g. "image/png"); URL is empty.
type ImageSource struct {
	Kind      string `json:"kind"`                 // "url" | "base64"
	URL       string `json:"url,omitempty"`        // when Kind=="url"
	MediaType string `json:"media_type,omitempty"` // when Kind=="base64"
	Data      string `json:"data,omitempty"`       // base64, no prefix, when Kind=="base64"
}

// TextBlock is a small constructor for a text ContentBlock.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ImageURLBlock constructs an image ContentBlock that references a remote URL.
func ImageURLBlock(url string) ContentBlock {
	return ContentBlock{Type: BlockImage, Image: &ImageSource{Kind: ImageKindURL, URL: url}}
}

// ImageBase64Block constructs an image ContentBlock holding inline base64 bytes.
// data must be base64 WITHOUT the "data:<mt>;base64," prefix; mediaType is the
// IANA media type (e.g. "image/png").
func ImageBase64Block(mediaType, data string) ContentBlock {
	return ContentBlock{Type: BlockImage, Image: &ImageSource{Kind: ImageKindBase64, MediaType: mediaType, Data: data}}
}

// ToolUseBlockOf constructs a tool_use ContentBlock.
func ToolUseBlockOf(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ToolUse: &ToolUseBlock{ID: id, Name: name, Input: input}}
}

// ToolResultBlockOf constructs a tool_result ContentBlock.
func ToolResultBlockOf(toolUseID string, content json.RawMessage, isError bool) ContentBlock {
	return ContentBlock{Type: BlockToolResult, ToolResult: &ToolResultBlock{ToolUseID: toolUseID, Content: content, IsError: isError}}
}

// IsImage reports whether the block is an image block with a populated source.
func (c ContentBlock) IsImage() bool { return c.Type == BlockImage && c.Image != nil }

// IsToolUse reports whether the block is a populated tool_use block.
func (c ContentBlock) IsToolUse() bool { return c.Type == BlockToolUse && c.ToolUse != nil }

// IsToolResult reports whether the block is a populated tool_result block.
func (c ContentBlock) IsToolResult() bool { return c.Type == BlockToolResult && c.ToolResult != nil }

// HasImages reports whether any block in the message is an image block. The
// adapters use this to decide between emitting a plain string content (text-only,
// byte-identical to the legacy path) and a multimodal parts array.
func (m Message) HasImages() bool {
	for _, c := range m.Content {
		if c.IsImage() {
			return true
		}
	}
	return false
}

// Message is a single conversational turn in the unified representation. System
// content is NOT stored here — it is hoisted into UnifiedRequest.System so each
// upstream can place it where it belongs (OpenAI: a system message; Anthropic /
// Bedrock: a dedicated top-level system field).
type Message struct {
	Role    string         `json:"role"` // RoleUser | RoleAssistant
	Content []ContentBlock `json:"content"`
}

// Text returns the concatenation of all text blocks in the message — convenient
// for upstreams (like OpenAI) whose message content is a single string.
func (m Message) Text() string {
	var b []byte
	for i, c := range m.Content {
		if i > 0 && len(b) > 0 && c.Text != "" {
			b = append(b, '\n')
		}
		b = append(b, c.Text...)
	}
	return string(b)
}

// UnifiedRequest is the provider-agnostic representation of an inbound request
// after it has been parsed from either OpenAI Chat Completions or Anthropic
// Messages. Adapters consume it to build the upstream request.
type UnifiedRequest struct {
	// Model is the model id to send upstream. Callers (relay) are expected to
	// have already applied any channel model_mapping before BuildRequest.
	Model string `json:"model"`
	// System is the merged system prompt (may be empty). When multiple system
	// blocks exist inbound they are joined with newlines.
	System string `json:"system,omitempty"`
	// Messages are the non-system conversational turns in order.
	Messages []Message `json:"messages"`

	// Stream requests a streaming response when true.
	Stream bool `json:"stream,omitempty"`

	// Sampling / limit parameters. Pointers distinguish "unset" from a zero
	// value so adapters only forward what the caller actually specified.
	MaxTokens     *int     `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`

	// Tools is the verbatim JSON of the inbound tool definitions (the Anthropic
	// `tools` array: each {name, description, input_schema}). It is kept as raw
	// JSON and forwarded unchanged to an Anthropic upstream so no schema detail is
	// lost. Nil when the caller defined no tools. ToolChoice is the verbatim
	// `tool_choice` directive (e.g. {"type":"auto"}), nil when unset.
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// UnifiedResponse is the provider-agnostic representation of a (non-streaming)
// upstream response, ready to be transformed back into the inbound wire format.
type UnifiedResponse struct {
	// Model echoes the model that produced the response when the upstream
	// reports it; otherwise the request model.
	Model string `json:"model"`
	// Content is the assistant message content (text blocks).
	Content []ContentBlock `json:"content"`
	// StopReason is the normalized finish reason.
	StopReason StopReason `json:"stop_reason"`
	// Usage holds token accounting (see Usage / usage.go).
	Usage Usage `json:"usage"`
}

// Text returns the concatenated assistant text of the response.
func (r UnifiedResponse) Text() string {
	var b []byte
	for i, c := range r.Content {
		if i > 0 && len(b) > 0 && c.Text != "" {
			b = append(b, '\n')
		}
		b = append(b, c.Text...)
	}
	return string(b)
}

// StreamChunk is the provider-agnostic representation of a single streamed
// event, parsed from one upstream chunk. The relay converts it to the inbound
// format's chunk via transform.go. A chunk may carry incremental text, a final
// stop reason, terminal usage, or just be a no-op keep-alive (Done=false,
// empty fields) which the caller can skip.
type StreamChunk struct {
	// Delta is incremental text produced in this chunk (may be empty).
	Delta string `json:"delta,omitempty"`
	// StopReason is set on the terminating chunk (else StopUnknown).
	StopReason StopReason `json:"stop_reason,omitempty"`
	// Usage is set when the upstream reports usage on/near the final chunk.
	// Nil when this chunk carries no usage.
	Usage *Usage `json:"usage,omitempty"`
	// Done marks the end-of-stream sentinel (OpenAI "[DONE]", Bedrock
	// messageStop/metadata, Anthropic message_stop).
	Done bool `json:"done,omitempty"`
	// Model, when known, identifies the producing model.
	Model string `json:"model,omitempty"`
	// UpstreamErr is set when this chunk carries a FATAL mid-stream upstream
	// error (e.g. a Bedrock ConverseStream *Exception event). It MUST be
	// signaled via this field rather than ParseStreamChunk's error return:
	// both pumps treat a non-nil parse error as a malformed-frame skip
	// (`if perr != nil { return }`), so an exception returned as an error is
	// silently swallowed and the stream still ends as a clean 200 with empty
	// body. When this field is non-nil the chunk is meaningful and the pump
	// must terminate the stream with an error outcome (status=error, mapped
	// http_status, error_message). The parse error return stays reserved for
	// genuine decode malformations (which remain skip-able).
	UpstreamErr *UpstreamError `json:"-"`

	// ToolCallDelta carries an incremental tool-call fragment when the upstream is
	// streaming a tool/function call (OpenAI tool_calls deltas, Bedrock toolUse +
	// input_json deltas). Nil for plain text chunks. The relay's cross-format
	// stream builders emit the matching wire events from it.
	ToolCallDelta *ToolCallDelta `json:"tool_call_delta,omitempty"`
}

// ToolCallDelta is one incremental piece of a streamed tool call. Index groups
// fragments of the same call (a turn may stream several calls). ID/Name arrive on
// the first fragment of a call; ArgsFragment carries a slice of the JSON
// arguments string that the client concatenates across fragments.
type ToolCallDelta struct {
	Index        int    `json:"index"`
	ID           string `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	ArgsFragment string `json:"args_fragment,omitempty"`
}

// Adapter builds upstream requests and parses upstream responses for one
// provider family. Implementations are stateless and safe for concurrent use;
// the per-channel credential is supplied via the channel + Decryptor on each
// BuildRequest call.
type Adapter interface {
	// Name identifies the adapter ("openai" / "bedrock").
	Name() string

	// BuildRequest constructs the upstream HTTP request (URL, method, headers
	// incl. auth, and JSON body) for the given unified request and channel. It
	// does NOT send the request. The returned request honors uni.Stream by
	// targeting the streaming endpoint / setting the stream flag.
	BuildRequest(ctx context.Context, uni UnifiedRequest, ch *model.Channel) (*http.Request, error)

	// ParseResponse parses a complete (non-streaming) upstream response into the
	// unified representation plus parsed Usage. The caller is responsible for
	// closing resp.Body. Non-2xx responses yield an *UpstreamError.
	ParseResponse(resp *http.Response) (UnifiedResponse, Usage, error)

	// ParseStreamChunk parses one upstream stream event into a unified chunk.
	//
	// eventType is the event discriminator: for Bedrock ConverseStream it is the
	// value of the AWS event-stream `:event-type` header (e.g. "contentBlockDelta",
	// "messageStop", "metadata", or a "...Exception"); for plain-SSE upstreams
	// (OpenAI) there is no such header and eventType is "" — those adapters ignore
	// it and parse the payload body. payload is the event body: for Bedrock the
	// UNWRAPPED inner JSON of the event (NOT wrapped under an outer key — AWS sends
	// e.g. {"delta":{"text":"…"}}, not {"contentBlockDelta":{…}}); for OpenAI the
	// bytes after the SSE "data: " prefix.
	//
	// The returned bool reports whether the chunk carried meaningful content
	// (false → caller may skip it). The error return is reserved for genuine
	// decode malformations (callers skip those); a FATAL upstream error (e.g. a
	// Bedrock *Exception event) is carried on StreamChunk.UpstreamErr with
	// meaningful=true so the pump terminates the stream instead of swallowing it.
	ParseStreamChunk(eventType string, payload []byte) (StreamChunk, bool, error)
}

// Decryptor abstracts decrypting a channel's stored upstream key. The concrete
// implementation is *service.ChannelService (its Decrypt method), letting the
// relay (T7) inject it without this package importing the service layer.
type Decryptor interface {
	Decrypt(ch *model.Channel) (string, error)
}

// For uses the channel type to return the matching adapter. Unknown types yield
// a nil adapter and false so the relay can surface a clean error.
func For(ch *model.Channel, dec Decryptor) (Adapter, bool) {
	switch ch.Type {
	case model.ChannelOpenAI:
		return NewOpenAIAdapter(dec), true
	case model.ChannelBedrock:
		return NewBedrockAdapter(dec), true
	case model.ChannelAnthropic:
		return NewAnthropicAdapter(dec), true
	default:
		return nil, false
	}
}

// intPtr / floatPtr are small helpers for building optional params in tests and
// transforms.
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
