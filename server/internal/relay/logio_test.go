package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/adapter"
)

func TestRenderInboundText(t *testing.T) {
	uni := adapter.UnifiedRequest{
		System: "be brief",
		Messages: []adapter.Message{
			{Role: adapter.RoleUser, Content: []adapter.ContentBlock{adapter.TextBlock("hi there")}},
			{Role: adapter.RoleAssistant, Content: []adapter.ContentBlock{
				adapter.ToolUseBlockOf("t1", "get_weather", json.RawMessage(`{"city":"SF"}`)),
			}},
			{Role: adapter.RoleUser, Content: []adapter.ContentBlock{
				adapter.ToolResultBlockOf("t1", json.RawMessage(`"72F"`), false),
			}},
		},
	}
	got := renderInboundText(uni)
	for _, want := range []string{"system: be brief", "user: hi there", "tool_use get_weather", `{"city":"SF"}`, "tool_result"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered input missing %q:\n%s", want, got)
		}
	}
}

func TestTruncateLog(t *testing.T) {
	if got := truncateLog("short"); got != "short" {
		t.Errorf("short string altered: %q", got)
	}
	big := strings.Repeat("x", logIOMaxBytes+500)
	got := truncateLog(big)
	if len(got) <= logIOMaxBytes || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("truncation wrong: len=%d suffix?=%v", len(got), strings.HasSuffix(got, "[truncated]"))
	}
	if len(got) > logIOMaxBytes+len("\n…[truncated]") {
		t.Errorf("truncated body too long: %d", len(got))
	}
}
