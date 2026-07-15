package adapter

import (
	"encoding/json"
	"testing"
)

// TestProbeText_ToolUse verifies an assistant turn that only calls tools renders
// to the trained [tool_use] marker form (input-then-name), not an empty string.
func TestProbeText_ToolUse(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []ContentBlock{
		TextBlock("让我查一下组件位置。"),
		ToolUseBlockOf("t1", "codebase_search", json.RawMessage(`{"query":"c-flow-container"}`)),
	}}
	got := m.ProbeText()
	want := "让我查一下组件位置。\n" + `[tool_use] {"input":{"query":"c-flow-container"},"name":"codebase_search"}`
	if got != want {
		t.Fatalf("ProbeText mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestProbeText_ToolUseOnly_NotEmpty is the core regression: a tool-only turn
// must NOT be empty (that was the w=0 bug).
func TestProbeText_ToolUseOnly_NotEmpty(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []ContentBlock{
		ToolUseBlockOf("t1", "write_file", json.RawMessage(`{"path":"a.ts"}`)),
	}}
	if got := m.ProbeText(); got == "" {
		t.Fatal("tool-only assistant turn rendered empty — probe would see no tool signal")
	}
	if m.Text() != "" {
		t.Fatalf("precondition: Text() should be empty for tool-only turn, got %q", m.Text())
	}
}

// TestProbeText_ToolResult verifies a tool_result renders to the [tool_result] marker.
func TestProbeText_ToolResult(t *testing.T) {
	m := Message{Role: RoleUser, Content: []ContentBlock{
		ToolResultBlockOf("t1", json.RawMessage(`{"json":{"text":"ok"}}`), false),
	}}
	want := `[tool_result] {"json":{"text":"ok"}}`
	if got := m.ProbeText(); got != want {
		t.Fatalf("ProbeText mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestProbeText_PlainText keeps plain text identical to Text().
func TestProbeText_PlainText(t *testing.T) {
	m := Message{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}}
	if got := m.ProbeText(); got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

// TestProbeText_EmptyToolInput defaults empty tool input to {}.
func TestProbeText_EmptyToolInput(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []ContentBlock{
		ToolUseBlockOf("t1", "noargs", json.RawMessage("")),
	}}
	want := `[tool_use] {"input":{},"name":"noargs"}`
	if got := m.ProbeText(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
