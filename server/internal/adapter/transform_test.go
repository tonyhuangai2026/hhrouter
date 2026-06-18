package adapter

import (
	"context"
	"encoding/json"
	"testing"
)

// ---- Inbound OpenAI ⇄ unified ----

func TestParseOpenAIRequest_SystemMergeAndParts(t *testing.T) {
	in := OpenAIChatInbound{
		Model:  "gpt-4o",
		Stream: true,
		Messages: []OpenAIInboundMessage{
			{Role: RoleSystem, Content: json.RawMessage(`"sys one"`)},
			{Role: RoleSystem, Content: json.RawMessage(`"sys two"`)},
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"text","text":"part A"},{"type":"text","text":"part B"}]`)},
			{Role: RoleAssistant, Content: json.RawMessage(`"reply"`)},
		},
	}
	uni := ParseOpenAIRequest(in)

	if uni.System != "sys one\nsys two" {
		t.Errorf("merged system = %q", uni.System)
	}
	if !uni.Stream {
		t.Error("stream not propagated")
	}
	if len(uni.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system hoisted out)", len(uni.Messages))
	}
	// multi-part content flattened
	if uni.Messages[0].Text() != "part A\npart B" {
		t.Errorf("user text = %q", uni.Messages[0].Text())
	}
	if uni.Messages[1].Role != RoleAssistant || uni.Messages[1].Text() != "reply" {
		t.Errorf("assistant msg = %+v", uni.Messages[1])
	}
}

func TestBuildOpenAIResponse_StopReasonAndUsage(t *testing.T) {
	r := UnifiedResponse{
		Model:      "gpt-4o",
		Content:    []ContentBlock{TextBlock("answer")},
		StopReason: StopMaxTokens,
		Usage:      Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
	out := BuildOpenAIResponse(r)
	choices := out["choices"].([]map[string]any)
	if choices[0]["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", choices[0]["finish_reason"])
	}
	msg := choices[0]["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Errorf("content = %v", msg["content"])
	}
	usage := out["usage"].(map[string]any)
	if usage["total_tokens"] != 7 {
		t.Errorf("total_tokens = %v", usage["total_tokens"])
	}
}

// ---- Inbound Anthropic ⇄ unified ----

func TestParseAnthropicRequest_SystemBlocksAndContentBlocks(t *testing.T) {
	in := AnthropicInbound{
		Model:  "claude-3-5-sonnet",
		System: json.RawMessage(`[{"type":"text","text":"sys a"},{"type":"text","text":"sys b"}]`),
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"text","text":"block1"},{"type":"text","text":"block2"}]`)},
			{Role: RoleAssistant, Content: json.RawMessage(`"plain string"`)},
		},
		MaxTokens: intPtr(50),
	}
	uni := ParseAnthropicRequest(in)

	if uni.System != "sys a\nsys b" {
		t.Errorf("system = %q", uni.System)
	}
	if len(uni.Messages) != 2 {
		t.Fatalf("messages = %d", len(uni.Messages))
	}
	if len(uni.Messages[0].Content) != 2 {
		t.Errorf("user content blocks = %d, want 2", len(uni.Messages[0].Content))
	}
	if uni.Messages[0].Content[1].Text != "block2" {
		t.Errorf("second block = %q", uni.Messages[0].Content[1].Text)
	}
	if uni.Messages[1].Text() != "plain string" {
		t.Errorf("assistant text = %q", uni.Messages[1].Text())
	}
	if uni.MaxTokens == nil || *uni.MaxTokens != 50 {
		t.Errorf("max_tokens = %v", uni.MaxTokens)
	}
}

func TestParseAnthropicRequest_StringSystem(t *testing.T) {
	in := AnthropicInbound{System: json.RawMessage(`"just a string"`)}
	if got := ParseAnthropicRequest(in).System; got != "just a string" {
		t.Errorf("system = %q", got)
	}
}

// Some clients (e.g. Claude Code) put a role="system" entry inside messages in
// addition to (or instead of) the top-level system field. Upstreams reject any
// messages[].role that is not user/assistant, so such entries must be hoisted
// into System rather than forwarded verbatim.
func TestParseAnthropicRequest_SystemRoleInMessagesHoisted(t *testing.T) {
	in := AnthropicInbound{
		Model:  "claude-opus-4-8",
		System: json.RawMessage(`"top system"`),
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`"hi"`)},
			{Role: RoleSystem, Content: json.RawMessage(`"You are Claude Code"`)},
		},
	}
	uni := ParseAnthropicRequest(in)

	if uni.System != "top system\nYou are Claude Code" {
		t.Errorf("system = %q, want merged top + hoisted", uni.System)
	}
	if len(uni.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (system stripped)", len(uni.Messages))
	}
	for _, m := range uni.Messages {
		if m.Role == RoleSystem {
			t.Errorf("system role leaked into messages: %+v", m)
		}
	}
	if uni.Messages[0].Role != RoleUser {
		t.Errorf("remaining message role = %q, want user", uni.Messages[0].Role)
	}
}

func TestBuildAnthropicResponse_MultiBlockAndStop(t *testing.T) {
	r := UnifiedResponse{
		Model:      "claude",
		Content:    []ContentBlock{TextBlock("a"), TextBlock("b")},
		StopReason: StopEndTurn,
		Usage:      Usage{PromptTokens: 5, CompletionTokens: 9},
	}
	out := BuildAnthropicResponse(r)
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", out["stop_reason"])
	}
	content := out["content"].([]map[string]any)
	if len(content) != 2 || content[1]["text"] != "b" {
		t.Errorf("content = %+v", content)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 5 || usage["output_tokens"] != 9 {
		t.Errorf("usage = %+v", usage)
	}
}

// ---- Cross-format round trip: Anthropic inbound → unified → OpenAI upstream shape ----

func TestCrossFormat_AnthropicToUnifiedToOpenAIResponse(t *testing.T) {
	in := AnthropicInbound{
		Model:    "claude",
		System:   json.RawMessage(`"sys"`),
		Messages: []AnthropicMessage{{Role: RoleUser, Content: json.RawMessage(`"q"`)}},
	}
	uni := ParseAnthropicRequest(in)
	// Simulate upstream OpenAI response coming back, re-rendered to Anthropic inbound.
	resp := UnifiedResponse{Model: uni.Model, Content: []ContentBlock{TextBlock("ans")}, StopReason: StopStopSequence}
	out := BuildAnthropicResponse(resp)
	if out["stop_reason"] != "stop_sequence" {
		t.Errorf("stop_reason = %v, want stop_sequence", out["stop_reason"])
	}
}

// ---- Unified ⇄ Bedrock Converse ----

func TestUnifiedToBedrock_SystemAndInference(t *testing.T) {
	uni := UnifiedRequest{
		Model:         "anthropic.claude",
		System:        "sys prompt",
		Messages:      []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("x"), TextBlock("y")}}},
		Temperature:   floatPtr(0.7),
		StopSequences: []string{"STOP"},
	}
	body, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	if len(body.System) != 1 || body.System[0].Text != "sys prompt" {
		t.Errorf("system = %+v", body.System)
	}
	if len(body.Messages[0].Content) != 2 {
		t.Errorf("content blocks = %d, want 2", len(body.Messages[0].Content))
	}
	if body.InferenceConfig == nil || body.InferenceConfig.Temperature == nil || *body.InferenceConfig.Temperature != 0.7 {
		t.Errorf("inferenceConfig = %+v", body.InferenceConfig)
	}
	if len(body.InferenceConfig.StopSequences) != 1 || body.InferenceConfig.StopSequences[0] != "STOP" {
		t.Errorf("stopSequences = %+v", body.InferenceConfig.StopSequences)
	}
}

func TestUnifiedToBedrock_NoInferenceWhenAllUnset(t *testing.T) {
	uni := UnifiedRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}}}
	body, err := unifiedToBedrock(context.Background(), uni)
	if err != nil {
		t.Fatalf("unifiedToBedrock: %v", err)
	}
	if body.InferenceConfig != nil {
		t.Errorf("inferenceConfig should be nil when no params set, got %+v", body.InferenceConfig)
	}
}

// TestUnifiedToBedrock_EmptyBlockFiltering is the §2.1 unit-level regression
// (Tech Design §2.1, fixes the Turn-2 ContentBlock error). It asserts at the
// transform boundary that: empty text blocks are dropped, messages left with no
// blocks are dropped entirely, image blocks are always kept (even alongside an
// empty text block), and non-empty text-only messages are byte-identical to
// before. wantMsgs lists, per surviving message, the expected text of each
// surviving block ("" sentinel via a nil entry means an image block).
func TestUnifiedToBedrock_EmptyBlockFiltering(t *testing.T) {
	img := ImageBase64Block("image/png", "QUJD") // "ABC"

	tests := []struct {
		name     string
		messages []Message
		// wantTexts[i] is the expected text of each text block in surviving
		// message i; a -1 sentinel length means "this message should be dropped"
		// and never appears here — only surviving messages are listed.
		wantTexts [][]string
		// wantImageAt[i] is the count of image blocks expected in surviving msg i.
		wantImages []int
	}{
		{
			name:       "non-empty text-only unchanged",
			messages:   []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
			wantTexts:  [][]string{{"hi"}},
			wantImages: []int{0},
		},
		{
			name: "empty text block dropped, real text kept",
			messages: []Message{{Role: RoleUser, Content: []ContentBlock{
				TextBlock(""), TextBlock("real"), TextBlock(""),
			}}},
			wantTexts:  [][]string{{"real"}},
			wantImages: []int{0},
		},
		{
			name: "message with only empty text is dropped entirely",
			messages: []Message{
				{Role: RoleUser, Content: []ContentBlock{TextBlock("q1")}},
				{Role: RoleAssistant, Content: []ContentBlock{TextBlock("")}}, // empty Turn-1 reply
				{Role: RoleUser, Content: []ContentBlock{TextBlock("q2")}},
			},
			wantTexts:  [][]string{{"q1"}, {"q2"}},
			wantImages: []int{0, 0},
		},
		{
			name: "image block kept even when paired with empty text",
			messages: []Message{{Role: RoleUser, Content: []ContentBlock{
				TextBlock(""), img,
			}}},
			wantTexts:  [][]string{{}}, // no text blocks survive
			wantImages: []int{1},
		},
		{
			name: "image + real text both kept, order preserved",
			messages: []Message{{Role: RoleUser, Content: []ContentBlock{
				TextBlock("see"), img, TextBlock(""),
			}}},
			wantTexts:  [][]string{{"see"}},
			wantImages: []int{1},
		},
		{
			name: "all messages empty → no messages on the wire",
			messages: []Message{
				{Role: RoleUser, Content: []ContentBlock{TextBlock("")}},
				{Role: RoleAssistant, Content: []ContentBlock{}},
			},
			wantTexts:  [][]string{},
			wantImages: []int{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := unifiedToBedrock(context.Background(), UnifiedRequest{Model: "m", Messages: tc.messages})
			if err != nil {
				t.Fatalf("unifiedToBedrock: %v", err)
			}
			if len(body.Messages) != len(tc.wantTexts) {
				t.Fatalf("surviving messages = %d, want %d: %+v", len(body.Messages), len(tc.wantTexts), body.Messages)
			}
			for i, m := range body.Messages {
				var gotTexts []string
				var gotImages int
				for _, blk := range m.Content {
					if blk.Image != nil {
						gotImages++
						if blk.Text != "" {
							t.Errorf("msg[%d]: image block also carries text %q", i, blk.Text)
						}
						continue
					}
					if blk.Text == "" {
						t.Errorf("msg[%d]: an empty {text:\"\"} block survived (Bedrock rejects it)", i)
					}
					gotTexts = append(gotTexts, blk.Text)
				}
				if len(gotTexts) != len(tc.wantTexts[i]) {
					t.Errorf("msg[%d] text blocks = %v, want %v", i, gotTexts, tc.wantTexts[i])
				} else {
					for j, want := range tc.wantTexts[i] {
						if gotTexts[j] != want {
							t.Errorf("msg[%d].text[%d] = %q, want %q", i, j, gotTexts[j], want)
						}
					}
				}
				if gotImages != tc.wantImages[i] {
					t.Errorf("msg[%d] image blocks = %d, want %d", i, gotImages, tc.wantImages[i])
				}
			}
		})
	}
}

// ---- Stop-reason mapping: bidirectional across all three formats ----

func TestStopReasonRoundTrips(t *testing.T) {
	cases := []struct {
		reason    StopReason
		openAI    string
		anthropic string
		bedrock   string
	}{
		{StopEndTurn, "stop", "end_turn", "end_turn"},
		{StopMaxTokens, "length", "max_tokens", "max_tokens"},
		{StopStopSequence, "stop", "stop_sequence", "stop_sequence"},
		{StopToolUse, "tool_calls", "tool_use", "tool_use"},
	}
	for _, c := range cases {
		if got := stopToOpenAIFinish(c.reason); got != c.openAI {
			t.Errorf("%s → openai = %q, want %q", c.reason, got, c.openAI)
		}
		if got := stopToAnthropic(c.reason); got != c.anthropic {
			t.Errorf("%s → anthropic = %q, want %q", c.reason, got, c.anthropic)
		}
		if got := stopToBedrock(c.reason); got != c.bedrock {
			t.Errorf("%s → bedrock = %q, want %q", c.reason, got, c.bedrock)
		}
		// Reverse: inbound finish strings → unified.
		if got := openAIFinishToStop(c.openAI); got != c.reason && c.reason != StopStopSequence {
			// "stop" maps to end_turn (ambiguous); skip stop_sequence reverse.
			t.Errorf("openai %q → unified = %q, want %q", c.openAI, got, c.reason)
		}
		if got := anthropicStopToUnified(c.anthropic); got != c.reason {
			t.Errorf("anthropic %q → unified = %q, want %q", c.anthropic, got, c.reason)
		}
		if got := bedrockStopToUnified(c.bedrock); got != c.reason {
			t.Errorf("bedrock %q → unified = %q, want %q", c.bedrock, got, c.reason)
		}
	}
}

func TestBedrockContentFilterStop(t *testing.T) {
	if got := bedrockStopToUnified("content_filtered"); got != StopContentFilter {
		t.Errorf("content_filtered → %q, want content_filter", got)
	}
	if got := bedrockStopToUnified("guardrail_intervened"); got != StopContentFilter {
		t.Errorf("guardrail_intervened → %q, want content_filter", got)
	}
}

// ---- Stream chunk conversion across formats ----

func TestBuildOpenAIStreamChunk(t *testing.T) {
	// text delta
	out := BuildOpenAIStreamChunk("m", StreamChunk{Delta: "Hel"})
	choices := out["choices"].([]map[string]any)
	delta := choices[0]["delta"].(map[string]any)
	if delta["content"] != "Hel" {
		t.Errorf("delta content = %v", delta["content"])
	}
	if choices[0]["finish_reason"] != nil {
		t.Errorf("finish_reason should be nil mid-stream, got %v", choices[0]["finish_reason"])
	}

	// finish chunk
	out = BuildOpenAIStreamChunk("m", StreamChunk{StopReason: StopMaxTokens})
	choices = out["choices"].([]map[string]any)
	if choices[0]["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", choices[0]["finish_reason"])
	}

	// usage-only chunk (Anthropic message_start prompt tokens, or include_usage
	// tail): must carry usage AND an EMPTY choices array. choices:[{delta:{}}]
	// makes strict clients (opencode) fail union validation.
	out = BuildOpenAIStreamChunk("m", StreamChunk{Usage: &Usage{TotalTokens: 12, PromptTokens: 12}})
	if out["usage"].(map[string]any)["total_tokens"] != 12 {
		t.Errorf("usage = %+v", out["usage"])
	}
	if uc := out["choices"].([]map[string]any); len(uc) != 0 {
		t.Errorf("usage-only chunk choices = %+v, want empty array", uc)
	}

	// pure [DONE]
	if BuildOpenAIStreamChunk("m", StreamChunk{Done: true}) != nil {
		t.Error("pure done chunk should map to nil (caller emits [DONE])")
	}
}

func TestBuildAnthropicStreamEvent(t *testing.T) {
	// text delta → content_block_delta
	ev, p, ok := BuildAnthropicStreamEvent(StreamChunk{Delta: "Hel"})
	if !ok || ev != "content_block_delta" {
		t.Fatalf("delta event = %q ok=%v", ev, ok)
	}
	d := p["delta"].(map[string]any)
	if d["type"] != "text_delta" || d["text"] != "Hel" {
		t.Errorf("delta payload = %+v", d)
	}

	// stop → message_delta with stop_reason + usage
	ev, p, ok = BuildAnthropicStreamEvent(StreamChunk{StopReason: StopEndTurn, Usage: &Usage{CompletionTokens: 9}})
	if !ok || ev != "message_delta" {
		t.Fatalf("stop event = %q ok=%v", ev, ok)
	}
	if p["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", p["delta"])
	}
	if p["usage"].(map[string]any)["output_tokens"] != 9 {
		t.Errorf("usage = %+v", p["usage"])
	}

	// nothing to emit
	if _, _, ok := BuildAnthropicStreamEvent(StreamChunk{Done: true}); ok {
		t.Error("pure done chunk should produce no anthropic event")
	}
}

// TestStreamChunk_BedrockToBothInbound exercises the full streaming path: a
// Bedrock contentBlockDelta upstream chunk converted into both OpenAI and
// Anthropic inbound chunk shapes (field mapping verification, both directions).
func TestStreamChunk_BedrockToBothInbound(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	c, ok, err := a.ParseStreamChunk("contentBlockDelta", []byte(`{"delta":{"text":"world"}}`))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}

	// → OpenAI
	oa := BuildOpenAIStreamChunk("m", c)
	if oa["choices"].([]map[string]any)[0]["delta"].(map[string]any)["content"] != "world" {
		t.Errorf("bedrock→openai delta lost: %+v", oa)
	}

	// → Anthropic
	ev, p, ok := BuildAnthropicStreamEvent(c)
	if !ok || ev != "content_block_delta" || p["delta"].(map[string]any)["text"] != "world" {
		t.Errorf("bedrock→anthropic delta lost: ev=%q p=%+v", ev, p)
	}
}

// TestStreamChunk_OpenAIToAnthropic exercises an OpenAI upstream delta → Anthropic inbound.
func TestStreamChunk_OpenAIToAnthropic(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	c, ok, _ := a.ParseStreamChunk("", []byte(`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`))
	if !ok {
		t.Fatal("expected meaningful chunk")
	}
	ev, p, ok := BuildAnthropicStreamEvent(c)
	if !ok || ev != "content_block_delta" || p["delta"].(map[string]any)["text"] != "hi" {
		t.Errorf("openai→anthropic delta lost: ev=%q p=%+v", ev, p)
	}
}

func TestDescribeRequest(t *testing.T) {
	// Exercises the debug helper so it is covered and not dead.
	s := describeRequest(UnifiedRequest{Model: "m", Messages: []Message{{}}})
	if s == "" {
		t.Error("describeRequest returned empty")
	}
}
