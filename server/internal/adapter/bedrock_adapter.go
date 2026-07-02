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

// BedrockAdapter targets the AWS Bedrock Runtime Converse API (Tech Design §6).
//
// Endpoint & auth — VERIFIED against AWS docs (2026-06):
//   - Non-stream: POST https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/converse
//   - Stream:     POST https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/converse-stream
//   - Auth: `Authorization: Bearer {decrypted channel.key}` using an AWS Bedrock
//     API key (the value of the AWS_BEARER_TOKEN_BEDROCK env var). NO SigV4 is
//     required — see https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html
//     ("Use an API key" → cURL example POSTs to /model/{id}/converse with exactly
//     this Authorization header). Bearer-only is therefore viable for the MVP and
//     we do NOT pull in aws-sdk-go-v2.
//
// Remaining uncertainty (documented for T7 / ops):
//   - The bearer key must be valid for the channel's region; short-term keys
//     expire (≤12h). Operators supplying expired/long-term keys will see 403s
//     surfaced as UpstreamError — this layer does not auto-refresh tokens.
//   - ConverseStream uses the AWS event-stream (vnd.amazon.eventstream) framing
//     over HTTP, NOT plain SSE. The relay de-frames the wire bytes, extracting
//     each event's `:event-type` header and its UNWRAPPED inner-JSON payload, and
//     passes both to ParseStreamChunk(eventType, payload) — e.g.
//     eventType="contentBlockDelta" with payload {"delta":{"text":"…"}}. The
//     legacy outer-wrapped shape ({"contentBlockDelta":{…}}) is only accepted on
//     the eventType=="" fallback. The unwrapped event shapes match aws-sdk-go-v2's
//     bedrockruntime ConverseStream union members 1:1, so an SDK-based relay maps
//     onto the same StreamChunk.
type BedrockAdapter struct {
	dec Decryptor
}

// NewBedrockAdapter constructs a BedrockAdapter.
func NewBedrockAdapter(dec Decryptor) *BedrockAdapter { return &BedrockAdapter{dec: dec} }

// Name implements Adapter.
func (a *BedrockAdapter) Name() string { return "bedrock" }

// ---- wire types (Bedrock Converse API) ----------------------------------

// bedrockContentBlock is one block of Converse message content: either a text
// block (Text set) or an image block (Image set). On the wire each block carries
// exactly one populated member.
type bedrockContentBlock struct {
	Text       string                  `json:"text,omitempty"`
	Image      *bedrockImageBlock      `json:"image,omitempty"`
	ToolUse    *bedrockToolUseBlock    `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResultBlock `json:"toolResult,omitempty"`
}

// bedrockToolUseBlock is a Converse toolUse content block: the model's request to
// call a tool. Input is the arguments JSON object.
type bedrockToolUseBlock struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// bedrockToolResultBlock is a Converse toolResult content block: the client's
// returned tool output. Content is a list of result sub-blocks (we emit a single
// {json} or {text} block).
type bedrockToolResultBlock struct {
	ToolUseID string           `json:"toolUseId"`
	Content   []map[string]any `json:"content"`
	Status    string           `json:"status,omitempty"`
}

// bedrockImageBlock is a Converse image content block:
// {image:{format:<png|jpeg|gif|webp>, source:{bytes:<base64>}}}.
type bedrockImageBlock struct {
	Format string             `json:"format"`
	Source bedrockImageSource `json:"source"`
}

// bedrockImageSource carries the inline base64 image bytes Converse requires.
type bedrockImageSource struct {
	Bytes string `json:"bytes"`
}

// bedrockMessage is one Converse message: a role and a list of content blocks.
type bedrockMessage struct {
	Role    string                `json:"role"` // "user" | "assistant"
	Content []bedrockContentBlock `json:"content"`
}

// bedrockSystemBlock is one Converse system content block (text only for MVP).
type bedrockSystemBlock struct {
	Text string `json:"text"`
}

// bedrockInferenceConfig maps the unified sampling/limit params.
type bedrockInferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// bedrockConverseRequest is the Converse / ConverseStream request body.
type bedrockConverseRequest struct {
	Messages        []bedrockMessage        `json:"messages"`
	System          []bedrockSystemBlock    `json:"system,omitempty"`
	InferenceConfig *bedrockInferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *bedrockToolConfig      `json:"toolConfig,omitempty"`
}

// bedrockToolConfig is the Converse toolConfig: the list of tool specs plus an
// optional toolChoice.
type bedrockToolConfig struct {
	Tools      []bedrockTool  `json:"tools"`
	ToolChoice map[string]any `json:"toolChoice,omitempty"`
}

// bedrockTool wraps one toolSpec ({name,description,inputSchema:{json:<schema>}}).
type bedrockTool struct {
	ToolSpec bedrockToolSpec `json:"toolSpec"`
}

type bedrockToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// bedrockUsage mirrors the Converse TokenUsage object. cacheReadInputTokens /
// cacheWriteInputTokens are the prompt-cache buckets (absent on models/regions
// without prompt caching → 0), priced on their own tiers.
type bedrockUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

// bedrockConverseResponse is the non-streaming Converse response.
type bedrockConverseResponse struct {
	Output struct {
		Message bedrockMessage `json:"message"`
	} `json:"output"`
	StopReason string        `json:"stopReason"`
	Usage      *bedrockUsage `json:"usage"`
	// Message is present on error bodies (e.g. ValidationException).
	Message string `json:"message"`
}

// ---- UNWRAPPED ConverseStream event bodies (the real AWS wire shape) --------
//
// On the real AWS ConverseStream the event discriminator lives in the
// vnd.amazon.eventstream `:event-type` HEADER and the frame PAYLOAD is the
// UNWRAPPED inner JSON of that event — e.g. for :event-type=contentBlockDelta
// the payload is {"contentBlockIndex":0,"delta":{"text":"…"}} (NOT wrapped under
// a "contentBlockDelta" key). These structs decode those unwrapped bodies; the
// adapter selects which one to use from the eventType passed by the relay's
// de-framer. Field tags verified against the Bedrock Converse API docs:
// contentBlockDelta→delta.text, messageStop→stopReason,
// metadata→usage.{inputTokens,outputTokens,totalTokens}.

// bedrockToolUseStart / bedrockToolUseDelta are the toolUse payload shapes seen
// on contentBlockStart / contentBlockDelta.
type bedrockToolUseStart struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
}

type bedrockToolUseDelta struct {
	Input string `json:"input"`
}

// bedrockContentBlockDelta is the UNWRAPPED contentBlockDelta event body. The
// delta carries either text OR a toolUse input fragment. Field placement varies
// by endpoint: the AWS Converse spec nests it under `delta`, but the live
// bedrock-runtime endpoint we target returns `toolUse`/`text` at the TOP LEVEL of
// the payload — so we accept both and prefer whichever is populated.
type bedrockContentBlockDelta struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	// spec shape: {"delta":{"text":..,"toolUse":{"input":..}}}
	Delta struct {
		Text    string               `json:"text"`
		ToolUse *bedrockToolUseDelta `json:"toolUse"`
	} `json:"delta"`
	// top-level shape (live endpoint): {"toolUse":{"input":..}} / {"text":..}
	Text    string               `json:"text"`
	ToolUse *bedrockToolUseDelta `json:"toolUse"`
}

// text resolves the delta text from whichever shape carried it.
func (d bedrockContentBlockDelta) text() string {
	if d.Delta.Text != "" {
		return d.Delta.Text
	}
	return d.Text
}

// toolInput resolves the toolUse input fragment from whichever shape carried it,
// with ok=false when this delta has no toolUse part.
func (d bedrockContentBlockDelta) toolInput() (string, bool) {
	if d.Delta.ToolUse != nil {
		return d.Delta.ToolUse.Input, true
	}
	if d.ToolUse != nil {
		return d.ToolUse.Input, true
	}
	return "", false
}

// bedrockContentBlockStart is the UNWRAPPED contentBlockStart event body. For a
// tool call it carries the toolUse spec (id + name). As with the delta, the
// toolUse object is nested under `start` per the AWS Converse spec but appears at
// the TOP LEVEL on the live endpoint, so we accept both.
type bedrockContentBlockStart struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	Start             struct {
		ToolUse *bedrockToolUseStart `json:"toolUse"`
	} `json:"start"`
	ToolUse *bedrockToolUseStart `json:"toolUse"`
}

// toolStart resolves the toolUse spec from whichever shape carried it.
func (s bedrockContentBlockStart) toolStart() *bedrockToolUseStart {
	if s.Start.ToolUse != nil {
		return s.Start.ToolUse
	}
	return s.ToolUse
}

// bedrockMessageStop is the UNWRAPPED messageStop event body.
type bedrockMessageStop struct {
	StopReason string `json:"stopReason"`
}

// bedrockMetadata is the UNWRAPPED metadata event body (carries terminal usage
// and, ignored here, metrics/trace).
type bedrockMetadata struct {
	Usage *bedrockUsage `json:"usage"`
}

// bedrockExceptionEvent is the UNWRAPPED body of a ConverseStream error event
// (e.g. validationException / throttlingException / modelStreamErrorException /
// internalServerException / serviceUnavailableException / modelTimeoutException).
// All of them carry a human-readable {message}.
type bedrockExceptionEvent struct {
	Message string `json:"message"`
}

// bedrockStreamEvent is one decoded ConverseStream event payload in the LEGACY
// outer-WRAPPED shape ({"contentBlockDelta":{…}}). Real AWS does NOT send this;
// it is retained ONLY for the eventType=="" fallback path (a non-standard /
// pre-deframed upstream that inlines the discriminator as an outer JSON key).
// Each event carries exactly one populated member (a union on the wire).
type bedrockStreamEvent struct {
	MessageStart *struct {
		Role string `json:"role"`
	} `json:"messageStart"`
	ContentBlockDelta *struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"contentBlockDelta"`
	ContentBlockStop *struct {
		ContentBlockIndex int `json:"contentBlockIndex"`
	} `json:"contentBlockStop"`
	MessageStop *struct {
		StopReason string `json:"stopReason"`
	} `json:"messageStop"`
	Metadata *struct {
		Usage *bedrockUsage `json:"usage"`
	} `json:"metadata"`
}

// ---- BuildRequest --------------------------------------------------------

// BuildRequest implements Adapter. It derives the regional Converse endpoint
// from the channel region and the unified model id, and authenticates with the
// channel's bearer key.
func (a *BedrockAdapter) BuildRequest(ctx context.Context, uni UnifiedRequest, ch *model.Channel) (*http.Request, error) {
	if strings.TrimSpace(ch.Region) == "" {
		return nil, fmt.Errorf("bedrock adapter: channel %d has no region", ch.ID)
	}
	if strings.TrimSpace(uni.Model) == "" {
		return nil, fmt.Errorf("bedrock adapter: empty model id")
	}
	key, err := a.dec.Decrypt(ch)
	if err != nil {
		return nil, fmt.Errorf("bedrock adapter: decrypt key: %w", err)
	}

	body, err := unifiedToBedrock(ctx, uni)
	if err != nil {
		return nil, fmt.Errorf("bedrock adapter: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock adapter: marshal body: %w", err)
	}

	effectiveModel := applyInferenceProfile(uni.Model, ch.Region, ch.UseInferenceProfile)
	endpoint := bedrockEndpoint(ch.Region, effectiveModel, uni.Stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if uni.Stream {
		// Bedrock streams the AWS event-stream content type.
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

// bedrockEndpoint builds the regional Converse[-stream] URL for a model id.
func bedrockEndpoint(region, modelID string, stream bool) string {
	op := "converse"
	if stream {
		op = "converse-stream"
	}
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s", region, modelID, op)
}

// ---- cross-region inference profile prefixing (Tech Design §3) -----------

// regionToProfilePrefix maps an AWS region to its cross-region inference-profile
// group prefix. Only the three documented inference-profile groups get a prefix:
// us-* → "us.", eu-* → "eu.", ap-* → "apac.". EVERYTHING ELSE — including ca-*,
// us-gov-*, empty, and unknown regions — returns "" (no prefixing). Canada and
// GovCloud are NOT members of the us. cross-region inference group; prefixing
// them would build an invalid profile id that fails hard, so the safe fallback
// for any non-us/eu/ap region is no prefix at all.
func regionToProfilePrefix(region string) string {
	switch {
	case strings.HasPrefix(region, "us-gov-"):
		// GovCloud is NOT in the us. cross-region inference group; must be
		// matched before the generic us- case so it falls through to no prefix.
		return ""
	case strings.HasPrefix(region, "us-"):
		return "us."
	case strings.HasPrefix(region, "eu-"):
		return "eu."
	case strings.HasPrefix(region, "ap-"):
		return "apac."
	default:
		return ""
	}
}

// alreadyProfilePrefixed reports whether modelID already starts with a
// cross-region inference-profile group segment (us.|eu.|apac.|global.) — in
// which case we must not prefix again.
func alreadyProfilePrefixed(modelID string) bool {
	for _, p := range []string{"us.", "eu.", "apac.", "global."} {
		if strings.HasPrefix(modelID, p) {
			return true
		}
	}
	return false
}

// applyInferenceProfile returns the model id to use in the Bedrock URL.
// It only prefixes when: the channel flag is on, the id isn't already
// profile-prefixed, the id looks like a bare provider id (contains a ".", e.g.
// anthropic.xxx / amazon.xxx / meta.xxx), and a region prefix is resolvable.
// Otherwise the id is returned unchanged. Pure & table-testable.
func applyInferenceProfile(modelID, region string, enabled bool) string {
	if !enabled || alreadyProfilePrefixed(modelID) {
		return modelID
	}
	// Bare provider ids contain a provider prefix segment like "anthropic.".
	if !strings.Contains(modelID, ".") {
		return modelID // not a provider-qualified id; leave as-is
	}
	prefix := regionToProfilePrefix(region)
	if prefix == "" {
		return modelID
	}
	return prefix + modelID
}

// ---- ParseResponse -------------------------------------------------------

// ParseResponse implements Adapter for the non-streaming Converse response.
func (a *BedrockAdapter) ParseResponse(resp *http.Response) (UnifiedResponse, Usage, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider:   a.Name(),
			StatusCode: resp.StatusCode,
			Message:    bedrockErrorMessage(raw, resp.Status),
			Body:       snippet(raw),
		}
	}

	var parsed bedrockConverseResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return UnifiedResponse{}, Usage{}, &UpstreamError{
			Provider: a.Name(),
			Message:  fmt.Sprintf("decode response: %v", err),
			Body:     snippet(raw),
		}
	}

	out := bedrockToUnifiedResponse(parsed)

	usage := usageFromBedrock(parsed.Usage)
	if !usage.HasUpstream {
		usage = estimateUsageFromText("", out.Text())
	}
	out.Usage = usage
	return out, usage, nil
}

// ParseStreamChunk implements Adapter for Bedrock ConverseStream.
//
// eventType is the value of the AWS event-stream `:event-type` header that the
// relay's de-framer extracted for this frame; payload is the UNWRAPPED inner
// JSON body of the event. Dispatch is by eventType:
//   - contentBlockDelta → {delta:{text}}      → incremental text
//   - messageStop        → {stopReason}        → terminal stop reason
//   - metadata           → {usage:{…}, metrics}→ terminal usage
//   - messageStart / contentBlockStart / contentBlockStop → structural, ignored
//   - *Exception (validation/throttling/modelStreamError/internalServer/
//     serviceUnavailable/modelTimeout/…) → FATAL: surfaced on StreamChunk.UpstreamErr
//     (NOT a returned error — both pumps skip parse errors) so the pump aborts the
//     stream with an error outcome instead of a silent 200-empty.
//
// eventType=="" means there was no `:event-type` header (a non-AWS / already
// outer-wrapped upstream); we fall back to the legacy wrapped-key parse for
// compatibility. A genuine JSON decode failure returns an error (skip-able).
func (a *BedrockAdapter) ParseStreamChunk(eventType string, payload []byte) (StreamChunk, bool, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return StreamChunk{}, false, nil
	}

	// No discriminator header → legacy outer-wrapped shape (compat fallback).
	if eventType == "" {
		return a.parseWrappedStreamEvent(trimmed)
	}

	// Exception events carry the discriminator in the header and a {message}
	// body. They are FATAL and must propagate via UpstreamErr, never as a parse
	// error (which the pumps swallow as a skipped malformed frame).
	if isBedrockExceptionEvent(eventType) {
		var ex bedrockExceptionEvent
		_ = json.Unmarshal(trimmed, &ex) // best-effort; message may be absent
		msg := ex.Message
		if msg == "" {
			msg = eventType
		}
		out := StreamChunk{
			Done: true,
			UpstreamErr: &UpstreamError{
				Provider:   a.Name(),
				StatusCode: bedrockExceptionStatus(eventType),
				Message:    msg,
			},
		}
		return out, true, nil
	}

	var out StreamChunk
	switch eventType {
	case "contentBlockStart":
		// A tool call begins here (toolUse.{toolUseId,name}); emit the opening
		// tool-call delta. Plain text blocks carry no toolUse payload.
		var ev bedrockContentBlockStart
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return out, false, nil // structural; ignore on decode trouble
		}
		if ts := ev.toolStart(); ts != nil {
			out.ToolCallDelta = &ToolCallDelta{
				Index: ev.ContentBlockIndex,
				ID:    ts.ToolUseID,
				Name:  ts.Name,
			}
			return out, true, nil
		}
		return out, false, nil
	case "contentBlockDelta":
		var ev bedrockContentBlockDelta
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return StreamChunk{}, false, fmt.Errorf("bedrock adapter: decode contentBlockDelta: %w", err)
		}
		if frag, ok := ev.toolInput(); ok {
			out.ToolCallDelta = &ToolCallDelta{Index: ev.ContentBlockIndex, ArgsFragment: frag}
			return out, true, nil
		}
		out.Delta = ev.text()
	case "messageStop":
		var ev bedrockMessageStop
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return StreamChunk{}, false, fmt.Errorf("bedrock adapter: decode messageStop: %w", err)
		}
		out.StopReason = bedrockStopToUnified(ev.StopReason)
		out.Done = true
	case "metadata":
		var ev bedrockMetadata
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return StreamChunk{}, false, fmt.Errorf("bedrock adapter: decode metadata: %w", err)
		}
		if ev.Usage != nil {
			u := usageFromBedrock(ev.Usage)
			if u.HasUpstream {
				out.Usage = &u
			}
		}
		// metadata is the final event of the stream.
		out.Done = true
	case "messageStart", "contentBlockStop":
		// Structural events with no inbound-visible payload.
		return out, false, nil
	default:
		// Unknown / future event types are ignored (forward-compatible).
		return out, false, nil
	}

	meaningful := out.Delta != "" || out.StopReason != StopUnknown || out.Usage != nil || out.Done
	return out, meaningful, nil
}

// parseWrappedStreamEvent is the legacy fallback for eventType=="" (no
// `:event-type` header): it decodes an outer-WRAPPED event ({"contentBlockDelta":
// {…}}). Real AWS never sends this shape; the path exists only for non-standard /
// pre-deframed upstreams. Behavior matches the pre-fix dispatch exactly.
func (a *BedrockAdapter) parseWrappedStreamEvent(trimmed []byte) (StreamChunk, bool, error) {
	var ev bedrockStreamEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		return StreamChunk{}, false, fmt.Errorf("bedrock adapter: decode stream event: %w", err)
	}

	var out StreamChunk
	switch {
	case ev.ContentBlockDelta != nil:
		out.Delta = ev.ContentBlockDelta.Delta.Text
	case ev.MessageStop != nil:
		out.StopReason = bedrockStopToUnified(ev.MessageStop.StopReason)
		out.Done = true
	case ev.Metadata != nil && ev.Metadata.Usage != nil:
		u := usageFromBedrock(ev.Metadata.Usage)
		if u.HasUpstream {
			out.Usage = &u
		}
		out.Done = true
	case ev.MessageStart != nil, ev.ContentBlockStop != nil:
		return out, false, nil
	default:
		return out, false, nil
	}

	meaningful := out.Delta != "" || out.StopReason != StopUnknown || out.Usage != nil || out.Done
	return out, meaningful, nil
}

// isBedrockExceptionEvent reports whether an :event-type names a ConverseStream
// error event. AWS names every modeled stream error with an "Exception" suffix
// (validationException, throttlingException, modelStreamErrorException,
// internalServerException, serviceUnavailableException, modelTimeoutException, …),
// so the suffix is the robust, forward-compatible discriminator.
func isBedrockExceptionEvent(eventType string) bool {
	return strings.HasSuffix(eventType, "Exception")
}

// bedrockExceptionStatus maps a ConverseStream exception event-type to the HTTP
// status the relay/test-chat should report. Throttling → 429, model/service/
// timeout transients → 503, everything else (validation, internal, unknown) →
// 500 by default; the relay clamps anything outside 4xx/5xx to 502.
func bedrockExceptionStatus(eventType string) int {
	switch eventType {
	case "throttlingException":
		return http.StatusTooManyRequests
	case "modelStreamErrorException", "serviceUnavailableException", "modelTimeoutException":
		return http.StatusServiceUnavailable
	case "validationException":
		return http.StatusBadRequest
	case "internalServerException":
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// bedrockErrorMessage extracts a readable message from a Converse error body.
func bedrockErrorMessage(raw []byte, status string) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return status
}
