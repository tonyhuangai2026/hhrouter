package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// cache_control_passthrough_test.go covers the REQUEST-SIDE pass-through of
// Anthropic prompt-cache breakpoints (cache_control:{type:"ephemeral"}) added by
// the implementation task: parsing them into the unified representation
// (ParseAnthropicRequest), re-emitting them to Bedrock as {cachePoint} blocks
// (unifiedToBedrock), and re-emitting them to an Anthropic upstream as
// cache_control blocks (BuildAnthropicContentBlock / BuildRequest). It also pins
// the two reviewer blockers: Blocker-1 (a pure-cachePoint Bedrock system block
// must NOT serialize a "text":"" key) and Blocker-2 (the Anthropic outbound
// `system` field is a JSON array when a system breakpoint is set, a JSON string
// otherwise). The counterpart cache_render_test.go / cache_usage_test.go cover
// the RESPONSE-side usage rendering/parsing; this file is the request side.

// ---- 1. Parse: ParseAnthropicRequest --------------------------------------

// TestParseAnthropicRequest_SystemBlockCacheControl asserts a system field in
// block-array form carrying cache_control sets uni.SystemCacheControl.
func TestParseAnthropicRequest_SystemBlockCacheControl(t *testing.T) {
	in := AnthropicInbound{
		Model:  "claude-x",
		System: json.RawMessage(`[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}]`),
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`"hi"`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	if uni.SystemCacheControl == nil {
		t.Fatal("system block cache_control → uni.SystemCacheControl must be non-nil")
	}
	if uni.SystemCacheControl.Type != "ephemeral" {
		t.Errorf("SystemCacheControl.Type = %q, want ephemeral", uni.SystemCacheControl.Type)
	}
	if strings.TrimSpace(uni.System) != "be brief" {
		t.Errorf("System text = %q, want %q", uni.System, "be brief")
	}
}

// TestParseAnthropicRequest_MessageBlockCacheControl asserts a message content
// block carrying cache_control sets the corresponding ContentBlock.CacheControl.
func TestParseAnthropicRequest_MessageBlockCacheControl(t *testing.T) {
	in := AnthropicInbound{
		Model: "claude-x",
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`[
				{"type":"text","text":"first"},
				{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}},
				{"type":"text","text":"third"}
			]`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	if len(uni.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(uni.Messages))
	}
	blocks := uni.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(blocks))
	}
	// Only the marked (second) block carries CacheControl; the others are nil.
	if blocks[0].CacheControl != nil {
		t.Errorf("block[0] should have no CacheControl, got %+v", blocks[0].CacheControl)
	}
	if blocks[1].CacheControl == nil {
		t.Fatal("block[1] (marked) must have non-nil CacheControl")
	}
	if blocks[1].CacheControl.Type != "ephemeral" {
		t.Errorf("block[1].CacheControl.Type = %q, want ephemeral", blocks[1].CacheControl.Type)
	}
	if blocks[2].CacheControl != nil {
		t.Errorf("block[2] should have no CacheControl, got %+v", blocks[2].CacheControl)
	}
	// A message-level breakpoint must NOT leak into the system-level marker.
	if uni.SystemCacheControl != nil {
		t.Errorf("message-block cache_control must not set SystemCacheControl, got %+v", uni.SystemCacheControl)
	}
}

// TestParseAnthropicRequest_NoCacheControl asserts a plain-string system and an
// unmarked message request leave ALL cache markers nil (backward-compat parse).
func TestParseAnthropicRequest_NoCacheControl(t *testing.T) {
	in := AnthropicInbound{
		Model:  "claude-x",
		System: json.RawMessage(`"be brief"`), // plain string
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
			{Role: RoleUser, Content: json.RawMessage(`"just text"`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	if uni.SystemCacheControl != nil {
		t.Errorf("plain-string system → SystemCacheControl must be nil, got %+v", uni.SystemCacheControl)
	}
	for mi, m := range uni.Messages {
		for bi, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("no-marker request: message[%d].block[%d].CacheControl must be nil, got %+v", mi, bi, b.CacheControl)
			}
		}
	}
}

// ---- 2. Bedrock render: unifiedToBedrock ----------------------------------

// TestUnifiedToBedrock_SystemCachePoint asserts a system-level breakpoint appends
// a trailing {cachePoint:{type:"default"}} system block after the text block, and
// that the pure-cachePoint block serializes WITHOUT a "text":"" key (Blocker-1).
func TestUnifiedToBedrock_SystemCachePoint(t *testing.T) {
	uni := UnifiedRequest{
		Model:              "claude-x",
		System:             "be brief",
		SystemCacheControl: &CacheControl{Type: "ephemeral"},
		Messages:           []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}
	out, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	if len(out.System) != 2 {
		t.Fatalf("system blocks = %d, want 2 (text + cachePoint)", len(out.System))
	}
	// Text block precedes the cachePoint block.
	if out.System[0].Text != "be brief" || out.System[0].CachePoint != nil {
		t.Errorf("system[0] should be the text block, got %+v", out.System[0])
	}
	if out.System[1].CachePoint == nil || out.System[1].CachePoint.Type != "default" {
		t.Fatalf("system[1] should be a CachePoint{type:default}, got %+v", out.System[1])
	}
	if out.System[1].Text != "" {
		t.Errorf("cachePoint system block should carry no text, got %q", out.System[1].Text)
	}

	// Blocker-1: the marshaled pure-cachePoint block must NOT contain a "text" key.
	raw, err := json.Marshal(out.System[1])
	if err != nil {
		t.Fatalf("marshal cachePoint system block: %v", err)
	}
	if bytes.Contains(raw, []byte(`"text"`)) {
		t.Errorf("pure-cachePoint system block must omit the text key (Blocker-1), got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"cachePoint"`)) {
		t.Errorf("pure-cachePoint system block must contain cachePoint, got %s", raw)
	}
}

// TestUnifiedToBedrock_MessageCachePoint asserts a message ContentBlock carrying
// CacheControl gets a {cachePoint} content block appended right after that block.
func TestUnifiedToBedrock_MessageCachePoint(t *testing.T) {
	marked := TextBlock("cached")
	marked.CacheControl = &CacheControl{Type: "ephemeral"}
	uni := UnifiedRequest{
		Model: "claude-x",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
			TextBlock("before"),
			marked,
			TextBlock("after"),
		}}},
	}
	out, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	blocks := out.Messages[0].Content
	// before, cached, cachePoint, after → 4 blocks.
	if len(blocks) != 4 {
		t.Fatalf("content blocks = %d, want 4 (before, cached, cachePoint, after): %+v", len(blocks), blocks)
	}
	if blocks[1].Text != "cached" || blocks[1].CachePoint != nil {
		t.Errorf("block[1] should be the anchor text block, got %+v", blocks[1])
	}
	if blocks[2].CachePoint == nil || blocks[2].CachePoint.Type != "default" {
		t.Fatalf("block[2] should be the CachePoint right after the anchor, got %+v", blocks[2])
	}
	if blocks[3].Text != "after" {
		t.Errorf("block[3] should be the trailing text, got %+v", blocks[3])
	}
}

// TestUnifiedToBedrock_NoMarkersNoCachePoint asserts a request with NO cache
// markers marshals to a bedrockConverseRequest whose JSON has no "cachePoint".
func TestUnifiedToBedrock_NoMarkersNoCachePoint(t *testing.T) {
	uni := UnifiedRequest{
		Model:    "claude-x",
		System:   "be brief",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}
	out, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if bytes.Contains(raw, []byte("cachePoint")) {
		t.Errorf("no-marker request must not contain cachePoint, got %s", raw)
	}
}

// ---- 3. Anthropic re-render: BuildAnthropicContentBlock / BuildRequest -----

// TestBuildAnthropicContentBlock_CacheControl asserts a block carrying
// CacheControl emits cache_control:{type:"ephemeral"} on the rendered block, and
// a block without it emits none.
func TestBuildAnthropicContentBlock_CacheControl(t *testing.T) {
	marked := TextBlock("cached")
	marked.CacheControl = &CacheControl{Type: "ephemeral"}

	block, ok := BuildAnthropicContentBlock(marked)
	if !ok {
		t.Fatal("BuildAnthropicContentBlock returned ok=false for a text block")
	}
	cc, present := block["cache_control"]
	if !present {
		t.Fatalf("marked block must emit cache_control, got %+v", block)
	}
	ccMap, isMap := cc.(map[string]any)
	if !isMap || ccMap["type"] != "ephemeral" {
		t.Errorf("cache_control = %+v, want {type:ephemeral}", cc)
	}

	// Unmarked block → no cache_control key.
	plain, ok := BuildAnthropicContentBlock(TextBlock("plain"))
	if !ok {
		t.Fatal("BuildAnthropicContentBlock returned ok=false for a plain text block")
	}
	if _, present := plain["cache_control"]; present {
		t.Errorf("unmarked block must NOT emit cache_control, got %+v", plain)
	}
}

// TestAnthropicBuildRequest_SystemArrayWithCacheControl asserts that when
// SystemCacheControl!=nil the marshaled outbound body's `system` is a JSON array
// whose first element carries cache_control (Blocker-2). Uses the same
// stubDecryptor + model.Channel scaffolding as anthropic_adapter_test.go.
func TestAnthropicBuildRequest_SystemArrayWithCacheControl(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelAnthropic, BaseURL: "https://api.anthropic.com"}
	uni := UnifiedRequest{
		Model:              "claude-x",
		System:             "be brief",
		SystemCacheControl: &CacheControl{Type: "ephemeral"},
		Messages:           []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	// Decode into a shape-agnostic map so we can inspect the JSON type of `system`.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	sysRaw, present := body["system"]
	if !present {
		t.Fatal("body has no system field")
	}
	// Must be a JSON array, not a string.
	var sysArr []map[string]any
	if err := json.Unmarshal(sysRaw, &sysArr); err != nil {
		t.Fatalf("system must be a JSON array when SystemCacheControl!=nil (Blocker-2), got %s: %v", sysRaw, err)
	}
	if len(sysArr) != 1 {
		t.Fatalf("system array len = %d, want 1: %s", len(sysArr), sysRaw)
	}
	if sysArr[0]["type"] != "text" || sysArr[0]["text"] != "be brief" {
		t.Errorf("system[0] = %+v, want text block 'be brief'", sysArr[0])
	}
	cc, ok := sysArr[0]["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("system[0].cache_control = %+v, want {type:ephemeral}", sysArr[0]["cache_control"])
	}
}

// TestAnthropicBuildRequest_SystemStringWhenNoCacheControl asserts that with no
// SystemCacheControl the marshaled body's `system` is a JSON string (Blocker-2's
// backward-compatible shape).
func TestAnthropicBuildRequest_SystemStringWhenNoCacheControl(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelAnthropic, BaseURL: "https://api.anthropic.com"}
	uni := UnifiedRequest{
		Model:    "claude-x",
		System:   "be brief",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(readBody(t, req), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	sysRaw, present := body["system"]
	if !present {
		t.Fatal("body has no system field")
	}
	var s string
	if err := json.Unmarshal(sysRaw, &s); err != nil {
		t.Fatalf("system must be a JSON string when no cache marker (Blocker-2), got %s: %v", sysRaw, err)
	}
	if s != "be brief" {
		t.Errorf("system string = %q, want %q", s, "be brief")
	}
}

// ---- 4. Backward-compat ----------------------------------------------------

// TestBackwardCompat_NoCacheBedrock asserts an ordinary (no cache_control)
// request through the full parse→bedrock pipeline yields a bedrockConverseRequest
// whose JSON contains no "cachePoint".
func TestBackwardCompat_NoCacheBedrock(t *testing.T) {
	in := AnthropicInbound{
		Model:  "claude-x",
		System: json.RawMessage(`"be brief"`),
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	out, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("cachePoint")) {
		t.Errorf("no-cache request must produce no cachePoint, got %s", raw)
	}
}

// TestBackwardCompat_NoCacheAnthropicSystemString asserts a no-cache
// anthropicOutboundRequest marshals `system` as a JSON string (not an array),
// byte-compatible with the legacy string field.
func TestBackwardCompat_NoCacheAnthropicSystemString(t *testing.T) {
	body := anthropicOutboundRequest{
		Model:     "claude-x",
		MaxTokens: 100,
		System:    "be brief", // no cache breakpoint → plain string
		Messages:  []anthropicOutboundMsg{{Role: RoleUser, Content: []map[string]any{{"type": "text", "text": "hi"}}}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sysRaw := decoded["system"]
	var s string
	if err := json.Unmarshal(sysRaw, &s); err != nil {
		t.Fatalf("no-cache anthropicOutboundRequest system must be a JSON string, got %s: %v", sysRaw, err)
	}
	if s != "be brief" {
		t.Errorf("system = %q, want %q", s, "be brief")
	}
	// Sanity: it is NOT an array.
	var arr []any
	if json.Unmarshal(sysRaw, &arr) == nil {
		t.Errorf("no-cache system should not decode as an array, got %s", sysRaw)
	}
}
