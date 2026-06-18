package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// ===================== OpenAI inbound: tools + tool turns =====================

func TestParseOpenAIRequest_ToolsAndToolCalls(t *testing.T) {
	in := OpenAIChatInbound{
		Model:      "gpt-4o",
		Tools:      json.RawMessage(`[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`),
		ToolChoice: json.RawMessage(`"auto"`),
		Messages: []OpenAIInboundMessage{
			{Role: RoleUser, Content: json.RawMessage(`"weather?"`)},
			{Role: RoleAssistant, ToolCalls: []openAIToolCall{{ID: "call_1", Type: "function", Function: struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}{Name: "get_weather", Arguments: json.RawMessage(`"{\"city\":\"SF\"}"`)}}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"72F"`)},
		},
	}
	uni := ParseOpenAIRequest(in)

	// tools → canonical (Anthropic) shape with input_schema.
	if !strings.Contains(string(uni.Tools), `"input_schema"`) || !strings.Contains(string(uni.Tools), `"get_weather"`) {
		t.Errorf("tools not canonicalized: %s", uni.Tools)
	}
	if strings.TrimSpace(string(uni.ToolChoice)) != `{"type":"auto"}` {
		t.Errorf("tool_choice = %s", uni.ToolChoice)
	}

	// assistant tool_calls → tool_use block with UNQUOTED input object.
	var foundToolUse, foundToolResult bool
	for _, m := range uni.Messages {
		for _, c := range m.Content {
			if c.IsToolUse() && c.ToolUse.ID == "call_1" && c.ToolUse.Name == "get_weather" {
				foundToolUse = true
				if strings.TrimSpace(string(c.ToolUse.Input)) != `{"city":"SF"}` {
					t.Errorf("tool_use input = %s, want unquoted object", c.ToolUse.Input)
				}
			}
			if c.IsToolResult() && c.ToolResult.ToolUseID == "call_1" {
				foundToolResult = true
			}
		}
	}
	if !foundToolUse {
		t.Error("assistant tool_calls not parsed into tool_use block")
	}
	if !foundToolResult {
		t.Error("role=tool message not parsed into tool_result block")
	}
}

// ===================== OpenAI outbound: tools + tool turns =====================

func TestOpenAIAdapter_BuildRequest_ToolsAndToolTurns(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelOpenAI, BaseURL: "https://api.example.com"}
	uni := UnifiedRequest{
		Model: "gpt-4o",
		Tools: json.RawMessage(`[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}]`),
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("weather?")}},
			{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlockOf("call_9", "get_weather", json.RawMessage(`{"city":"SF"}`))}},
			{Role: RoleUser, Content: []ContentBlock{ToolResultBlockOf("call_9", json.RawMessage(`"72F"`), false)}},
		},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body := readBody(t, req)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	// tools → OpenAI function shape.
	tools, _ := json.Marshal(parsed["tools"])
	if !strings.Contains(string(tools), `"type":"function"`) || !strings.Contains(string(tools), `"parameters"`) {
		t.Errorf("tools not in OpenAI shape: %s", tools)
	}

	msgs := parsed["messages"].([]any)
	// Find the assistant message with tool_calls and the role=tool message.
	var sawAssistantToolCall, sawToolMsg bool
	for _, mi := range msgs {
		m := mi.(map[string]any)
		if m["role"] == RoleAssistant {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				tc := tcs[0].(map[string]any)
				fn := tc["function"].(map[string]any)
				if fn["name"] == "get_weather" && fn["arguments"] == `{"city":"SF"}` {
					sawAssistantToolCall = true
				}
			}
		}
		if m["role"] == "tool" && m["tool_call_id"] == "call_9" {
			sawToolMsg = true
			if m["content"] != "72F" {
				t.Errorf("tool message content = %v, want 72F", m["content"])
			}
		}
	}
	if !sawAssistantToolCall {
		t.Error("assistant tool_calls not rendered to OpenAI body")
	}
	if !sawToolMsg {
		t.Error("tool_result not rendered as role=tool message")
	}
}

func TestOpenAIAdapter_ParseResponse_ToolCalls(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	body := `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_5","type":"function","function":{"name":"calc","arguments":"{\"x\":2}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	uni, _, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if uni.StopReason != StopToolUse {
		t.Errorf("stop = %q, want tool_use", uni.StopReason)
	}
	var ok bool
	for _, c := range uni.Content {
		if c.IsToolUse() && c.ToolUse.Name == "calc" && strings.TrimSpace(string(c.ToolUse.Input)) == `{"x":2}` {
			ok = true
		}
	}
	if !ok {
		t.Errorf("tool_calls not parsed: %+v", uni.Content)
	}
}

func TestOpenAIAdapter_ParseStreamChunk_ToolCallDelta(t *testing.T) {
	a := NewOpenAIAdapter(stubDecryptor{})
	payload := []byte(`{"model":"gpt-4o","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":null}]}`)
	chunk, meaningful, err := a.ParseStreamChunk("", payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !meaningful || chunk.ToolCallDelta == nil {
		t.Fatalf("tool call delta not parsed: %+v", chunk)
	}
	if chunk.ToolCallDelta.ID != "call_1" || chunk.ToolCallDelta.Name != "f" || chunk.ToolCallDelta.ArgsFragment != `{"a":` {
		t.Errorf("tool call delta wrong: %+v", chunk.ToolCallDelta)
	}
}

// ===================== Bedrock outbound: toolConfig + tool turns =============

func TestBedrockAdapter_BuildRequest_ToolConfig(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model: "anthropic.claude",
		Tools: json.RawMessage(`[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`),
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{ToolUseBlockOf("tu_1", "get_weather", json.RawMessage(`{"city":"SF"}`))}},
			{Role: RoleUser, Content: []ContentBlock{ToolResultBlockOf("tu_1", json.RawMessage(`"72F"`), false)}},
		},
	}
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(readBody(t, req), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tc, _ := json.Marshal(parsed["toolConfig"])
	if !strings.Contains(string(tc), `"toolSpec"`) || !strings.Contains(string(tc), `"inputSchema"`) || !strings.Contains(string(tc), `"get_weather"`) {
		t.Errorf("toolConfig not built: %s", tc)
	}
	msgs, _ := json.Marshal(parsed["messages"])
	if !strings.Contains(string(msgs), `"toolUse"`) || !strings.Contains(string(msgs), `"toolResult"`) {
		t.Errorf("tool blocks not rendered to bedrock messages: %s", msgs)
	}
}

func TestBedrockAdapter_ParseResponse_ToolUse(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	body := `{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu_9","name":"calc","input":{"x":2}}}]}},` +
		`"stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	uni, _, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if uni.StopReason != StopToolUse {
		t.Errorf("stop = %q", uni.StopReason)
	}
	var ok bool
	for _, c := range uni.Content {
		if c.IsToolUse() && c.ToolUse.Name == "calc" && c.ToolUse.ID == "tu_9" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("toolUse not parsed: %+v", uni.Content)
	}
}

func TestBedrockAdapter_ParseStreamChunk_ToolUse(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	// contentBlockStart opens the tool call; contentBlockDelta streams input json.
	start, m1, err := a.ParseStreamChunk("contentBlockStart", []byte(`{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu_1","name":"f"}}}`))
	if err != nil || !m1 || start.ToolCallDelta == nil || start.ToolCallDelta.Name != "f" {
		t.Fatalf("contentBlockStart toolUse not parsed: %+v err=%v", start, err)
	}
	delta, m2, err := a.ParseStreamChunk("contentBlockDelta", []byte(`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"a\":1}"}}}`))
	if err != nil || !m2 || delta.ToolCallDelta == nil || delta.ToolCallDelta.ArgsFragment != `{"a":1}` {
		t.Fatalf("contentBlockDelta toolUse input not parsed: %+v err=%v", delta, err)
	}
}

// ===================== Cross-format response builders =========================

func TestBuildOpenAIResponse_ToolCalls(t *testing.T) {
	r := UnifiedResponse{
		Model:      "gpt-4o",
		StopReason: StopToolUse,
		Content:    []ContentBlock{ToolUseBlockOf("call_1", "get_weather", json.RawMessage(`{"city":"SF"}`))},
	}
	out := BuildOpenAIResponse(r)
	choice := out["choices"].([]map[string]any)[0]
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls missing: %+v", msg)
	}
	if msg["content"] != nil {
		t.Errorf("content = %v, want nil for tool-only turn", msg["content"])
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Errorf("function = %+v", fn)
	}
}

func TestBuildOpenAIStreamChunk_ToolCallDelta(t *testing.T) {
	c := StreamChunk{ToolCallDelta: &ToolCallDelta{Index: 0, ID: "call_1", Name: "f", ArgsFragment: `{"a":`}}
	out := BuildOpenAIStreamChunk("gpt-4o", c)
	if out == nil {
		t.Fatal("nil chunk for tool delta")
	}
	delta := out["choices"].([]map[string]any)[0]["delta"].(map[string]any)
	tcs, ok := delta["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls delta missing: %+v", delta)
	}
	if tcs[0]["id"] != "call_1" {
		t.Errorf("tool call id = %v", tcs[0]["id"])
	}
}
