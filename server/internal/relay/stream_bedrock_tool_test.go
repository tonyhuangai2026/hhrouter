package relay

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
)

// bedrockFrames encodes a sequence of (eventType, jsonPayload) into a single
// AWS event-stream body, mimicking a Bedrock converse-stream response.
func bedrockFrames(evs [][2]string) io.Reader {
	var buf bytes.Buffer
	for _, e := range evs {
		buf.Write(adapter.EncodeBedrockFrame(e[0], []byte(e[1])))
	}
	return &buf
}

// TestPumpStream_BedrockToolUse_ToAnthropic is the regression for Claude Code
// tool calls over a Bedrock channel: a Bedrock converse-stream tool call (top-
// level toolUse shape, as the live endpoint returns) must be reshaped into a
// proper Anthropic tool_use content block — content_block_start{tool_use} +
// input_json_delta + content_block_stop — not silently dropped.
func TestPumpStream_BedrockToolUse_ToAnthropic(t *testing.T) {
	rec := newCloseNotifyRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	// A realistic Bedrock tool-call stream (top-level toolUse, like the live API).
	body := bedrockFrames([][2]string{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Let me check."}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"contentBlockStart", `{"contentBlockIndex":1,"toolUse":{"name":"get_weather","toolUseId":"tooluse_x","type":"tool_use"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"toolUse":{"input":"{\"city\":"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"toolUse":{"input":"\"SF\"}"}}`},
		{"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"tool_use"}`},
		{"metadata", `{"usage":{"inputTokens":40,"outputTokens":12,"totalTokens":52}}`},
	})
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(body)}

	rc := &requestContext{outFormat: OutAnthropic, uni: adapter.UnifiedRequest{Model: "claude-opus-4-8"}}
	r := &Relayer{}
	_, _, _, err := r.pumpStream(c, rc, adapter.NewBedrockAdapter(stubDecryptorBR{}), resp, time.Now())
	if err != nil {
		t.Fatalf("pumpStream: %v", err)
	}

	out := rec.Body.String()
	// The tool_use block must reach the client with its name and id.
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Errorf("tool_use content_block_start missing:\n%s", out)
	}
	if !strings.Contains(out, `"id":"tooluse_x"`) {
		t.Errorf("tool_use id missing:\n%s", out)
	}
	// The streamed arguments must be forwarded as input_json_delta.
	if !strings.Contains(out, "input_json_delta") || !strings.Contains(out, `\"city\":`) {
		t.Errorf("input_json_delta arguments missing:\n%s", out)
	}
	// The leading text must still be there as a text block.
	if !strings.Contains(out, `"text_delta"`) || !strings.Contains(out, "Let me check.") {
		t.Errorf("leading text delta missing:\n%s", out)
	}
	// Framing: message_start ... message_stop, and stop_reason tool_use.
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_stop", "tool_use"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing framing %q", want)
		}
	}
	// EXACTLY ONE message_delta, carrying stop_reason=tool_use (Bedrock splits
	// stop/usage across messageStop+metadata; a second message_delta would clobber
	// the tool_use stop reason with end_turn).
	if n := strings.Count(out, "event: message_delta"); n != 1 {
		t.Errorf("message_delta count = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("final stop_reason not tool_use:\n%s", out)
	}
	if strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason was clobbered to end_turn:\n%s", out)
	}
}
