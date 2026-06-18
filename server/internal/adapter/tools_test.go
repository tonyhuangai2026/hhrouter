package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Tool calling support: these tests pin the inbound→unified→outbound round-trip
// for the Anthropic path that Claude Code uses. Before this support, tools were
// dropped on parse and tool_use/tool_result blocks were flattened to empty text,
// which made any agent client hang on the first tool call.

// Inbound Anthropic requests carry a top-level `tools` array and `tool_choice`.
// Both must survive verbatim into the UnifiedRequest.
func TestParseAnthropicRequest_ToolsForwarded(t *testing.T) {
	tools := `[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`
	choice := `{"type":"auto"}`
	in := AnthropicInbound{
		Model:      "claude-opus-4-8",
		Tools:      json.RawMessage(tools),
		ToolChoice: json.RawMessage(choice),
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`"weather in SF?"`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	if strings.TrimSpace(string(uni.Tools)) != tools {
		t.Errorf("tools not forwarded verbatim:\n got %s\nwant %s", uni.Tools, tools)
	}
	if strings.TrimSpace(string(uni.ToolChoice)) != choice {
		t.Errorf("tool_choice = %s, want %s", uni.ToolChoice, choice)
	}
}

// A multi-turn agent conversation includes an assistant tool_use block and a
// following user tool_result block. Both must parse into typed unified blocks.
func TestParseAnthropicRequest_ToolUseAndResultBlocks(t *testing.T) {
	in := AnthropicInbound{
		Model: "claude-opus-4-8",
		Messages: []AnthropicMessage{
			{Role: RoleUser, Content: json.RawMessage(`"weather?"`)},
			{Role: RoleAssistant, Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}]`)},
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"72F"}]`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	if len(uni.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(uni.Messages))
	}

	tu := uni.Messages[1].Content
	if len(tu) != 1 || !tu[0].IsToolUse() {
		t.Fatalf("assistant block is not tool_use: %+v", tu)
	}
	if tu[0].ToolUse.ID != "toolu_1" || tu[0].ToolUse.Name != "get_weather" {
		t.Errorf("tool_use id/name = %q/%q", tu[0].ToolUse.ID, tu[0].ToolUse.Name)
	}
	if strings.TrimSpace(string(tu[0].ToolUse.Input)) != `{"city":"SF"}` {
		t.Errorf("tool_use input = %s", tu[0].ToolUse.Input)
	}

	tr := uni.Messages[2].Content
	if len(tr) != 1 || !tr[0].IsToolResult() {
		t.Fatalf("user block is not tool_result: %+v", tr)
	}
	if tr[0].ToolResult.ToolUseID != "toolu_1" {
		t.Errorf("tool_result tool_use_id = %q", tr[0].ToolResult.ToolUseID)
	}
}

// Building an Anthropic outbound content block from a unified tool_use / tool_result
// must produce the Anthropic wire shape.
func TestBuildAnthropicContentBlock_ToolUseAndResult(t *testing.T) {
	tu := ToolUseBlockOf("toolu_9", "search", json.RawMessage(`{"q":"go"}`))
	block, ok := BuildAnthropicContentBlock(tu)
	if !ok {
		t.Fatal("tool_use block not built")
	}
	if block["type"] != "tool_use" || block["id"] != "toolu_9" || block["name"] != "search" {
		t.Errorf("tool_use block wrong: %+v", block)
	}

	tr := ToolResultBlockOf("toolu_9", json.RawMessage(`"results here"`), false)
	rblock, ok := BuildAnthropicContentBlock(tr)
	if !ok {
		t.Fatal("tool_result block not built")
	}
	if rblock["type"] != "tool_result" || rblock["tool_use_id"] != "toolu_9" {
		t.Errorf("tool_result block wrong: %+v", rblock)
	}

	// Empty tool_use input must default to {} (Anthropic rejects null input).
	empty, _ := BuildAnthropicContentBlock(ToolUseBlockOf("x", "noop", nil))
	if got := strings.TrimSpace(string(empty["input"].(json.RawMessage))); got != "{}" {
		t.Errorf("empty input default = %q, want {}", got)
	}
}

// A unified response carrying a tool_use block must render into the inbound
// Anthropic response body as a tool_use content block (not flattened to text).
func TestBuildAnthropicResponse_ToolUse(t *testing.T) {
	r := UnifiedResponse{
		Model:      "claude-opus-4-8",
		StopReason: StopToolUse,
		Content: []ContentBlock{
			TextBlock("Let me check."),
			ToolUseBlockOf("toolu_3", "get_weather", json.RawMessage(`{"city":"NYC"}`)),
		},
	}
	out := BuildAnthropicResponse(r)
	content, ok := out["content"].([]map[string]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %+v", out["content"])
	}
	if content[0]["type"] != "text" {
		t.Errorf("block 0 type = %v, want text", content[0]["type"])
	}
	if content[1]["type"] != "tool_use" || content[1]["name"] != "get_weather" {
		t.Errorf("block 1 = %+v, want tool_use get_weather", content[1])
	}
	if out["stop_reason"] != stopToAnthropic(StopToolUse) {
		t.Errorf("stop_reason = %v", out["stop_reason"])
	}
}

// Round-trip: an inbound tool_use/tool_result conversation parsed and then rebuilt
// for an Anthropic upstream must preserve the tool blocks (not drop them).
func TestRoundTrip_AnthropicToolConversation(t *testing.T) {
	in := AnthropicInbound{
		Model: "claude-opus-4-8",
		Messages: []AnthropicMessage{
			{Role: RoleAssistant, Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]`)},
			{Role: RoleUser, Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)},
		},
	}
	uni := ParseAnthropicRequest(in)
	rebuilt := BuildAnthropicContentBlocks(uni.Messages[0].Content)
	if len(rebuilt) != 1 || rebuilt[0]["type"] != "tool_use" || rebuilt[0]["id"] != "t1" {
		t.Errorf("assistant rebuild lost tool_use: %+v", rebuilt)
	}
	rebuiltRes := BuildAnthropicContentBlocks(uni.Messages[1].Content)
	if len(rebuiltRes) != 1 || rebuiltRes[0]["type"] != "tool_result" || rebuiltRes[0]["tool_use_id"] != "t1" {
		t.Errorf("user rebuild lost tool_result: %+v", rebuiltRes)
	}
}

// The non-stream upstream Anthropic response parser must surface tool_use blocks
// into the unified response.
func TestAnthropicAdapter_ParseResponse_ToolUse(t *testing.T) {
	a := NewAnthropicAdapter(stubDecryptor{})
	body := `{"type":"message","model":"claude-opus-4-8","stop_reason":"tool_use",` +
		`"content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_5","name":"calc","input":{"x":2}}],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	uni, _, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawToolUse bool
	for _, c := range uni.Content {
		if c.IsToolUse() && c.ToolUse.Name == "calc" && c.ToolUse.ID == "toolu_5" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Errorf("tool_use not parsed from response: %+v", uni.Content)
	}
	if uni.StopReason != StopToolUse {
		t.Errorf("stop_reason = %q, want tool_use", uni.StopReason)
	}
}
