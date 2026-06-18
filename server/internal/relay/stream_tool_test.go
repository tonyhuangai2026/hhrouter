package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// closeNotifyRecorder wraps httptest.ResponseRecorder to satisfy the
// http.CloseNotifier + http.Flusher interfaces that gin's c.Stream requires
// (the bare recorder implements neither, so c.Stream panics without this).
type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
	closed chan bool
}

func newCloseNotifyRecorder() *closeNotifyRecorder {
	return &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closed: make(chan bool, 1)}
}

func (c *closeNotifyRecorder) CloseNotify() <-chan bool { return c.closed }
func (c *closeNotifyRecorder) Flush()                   { c.ResponseRecorder.Flush() }

// A realistic Anthropic tool-call SSE stream: message_start (with usage),
// content_block_start for a tool_use block, two input_json_delta chunks streaming
// the arguments, content_block_stop, then message_delta (stop_reason=tool_use +
// output usage) and message_stop. This is exactly the shape that the old
// reshaping pump collapsed to a single empty text block — making Claude Code hang.
const anthropicToolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":50,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

// pumpAnthropicPassthrough must forward the tool_use framing verbatim so an
// Anthropic client receives a usable multi-block tool-call stream, and must still
// sniff usage + stop for billing/logging.
func TestPumpAnthropicPassthrough_ToolCallStream(t *testing.T) {
	rec := newCloseNotifyRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(anthropicToolStream)),
	}

	r := &Relayer{}
	usage, completion, _, err := r.pumpAnthropicPassthrough(c, resp, time.Now())
	if err != nil {
		t.Fatalf("passthrough returned error: %v", err)
	}

	out := rec.Body.String()

	// The tool_use content_block_start MUST reach the client (this is what was lost).
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Errorf("tool_use block not forwarded to client:\n%s", out)
	}
	// The streamed arguments (input_json_delta) must survive.
	if !strings.Contains(out, "input_json_delta") || !strings.Contains(out, `\"city\":`) {
		t.Errorf("input_json_delta arguments not forwarded:\n%s", out)
	}
	// Framing the client needs to parse the stream must be present and reconstructed.
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing framing %q in client stream", want)
		}
	}

	// Usage must be sniffed for billing: input 50 (from message_start), output 15
	// (from message_delta).
	if usage == nil {
		t.Fatal("usage not sniffed")
	}
	if usage.PromptTokens != 50 {
		t.Errorf("prompt tokens = %d, want 50", usage.PromptTokens)
	}
	if usage.CompletionTokens != 15 {
		t.Errorf("completion tokens = %d, want 15", usage.CompletionTokens)
	}
	// A tool-only turn has no assistant text — completion text is legitimately empty.
	if completion != "" {
		t.Errorf("completion text = %q, want empty for tool-only turn", completion)
	}
}

// A plain text Anthropic stream must also pass through, with text sniffed for the
// completion fallback and TTFT.
func TestPumpAnthropicPassthrough_TextStream(t *testing.T) {
	const textStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	rec := newCloseNotifyRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(textStream))}

	r := &Relayer{}
	_, completion, firstToken, err := r.pumpAnthropicPassthrough(c, resp, time.Now())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if completion != "Hello world" {
		t.Errorf("completion = %q, want %q", completion, "Hello world")
	}
	if firstToken == nil {
		t.Error("TTFT not captured for text stream")
	}
	if !strings.Contains(rec.Body.String(), `"text":"Hello"`) {
		t.Errorf("text delta not forwarded:\n%s", rec.Body.String())
	}
}
