package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
)

// cache_render_test.go is the relay-side (package relay, white-box, NO DB/Redis)
// proof that prompt-cache tokens flow all the way from a Bedrock upstream through
// the PRODUCTION reformat paths into the client-facing Anthropic usage — both
// non-streaming (buildResponse → BuildAnthropicResponse) and streaming
// (pumpStream's hand-built terminal message_delta). It complements the adapter
// unit tests (cache_render_test.go there) and the DB-backed //go:build e2e suite
// (cache_e2e_test.go), and — like stream_bedrock_test.go — feeds REAL AWS
// event-stream frames through the production de-framer + adapter dispatch.
//
// Cache tokens on the wire: cacheReadInputTokens=30, cacheWriteInputTokens=20
// (Bedrock native names), which must render as Anthropic
// cache_read_input_tokens=30 / cache_creation_input_tokens=20.

// runPumpStreamRaw drives the PRODUCTION stream.go pumpStream over a real TCP
// listener (gin's c.Stream needs a flushing http writer) with the given OUTPUT
// format, serving the supplied Bedrock event-stream wire bytes, and returns the
// RAW streamed body the client received (plus the captured finalUsage). Modeled
// on runPumpStream in stream_bedrock_test.go, but parameterised on outFormat and
// returning the raw SSE so cache rendering on the terminal frame can be asserted.
func runPumpStreamRaw(t *testing.T, out OutputFormat, wire []byte) (body string, usage *adapter.Usage) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(wire)
	}))
	defer upstream.Close()

	r := &Relayer{}
	rc := &requestContext{format: FormatOpenAI, outFormat: out, uni: adapter.UnifiedRequest{Model: "anthropic.claude"}}
	ad := adapter.NewBedrockAdapter(stubDecryptorBR{})

	front := gin.New()
	front.POST("/run", func(c *gin.Context) {
		resp, err := http.Get(upstream.URL)
		if err != nil {
			t.Errorf("get upstream: %v", err)
			return
		}
		defer resp.Body.Close()
		setSSEHeaders(c)
		usage, _, _, _ = r.pumpStream(c, rc, ad, resp, time.Now())
	})
	frontSrv := httptest.NewServer(front)
	defer frontSrv.Close()

	resp, err := http.Post(frontSrv.URL+"/run", "application/json", nil)
	if err != nil {
		t.Fatalf("post front: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(raw), usage
}

// cacheBedrockStreamWire builds a full Bedrock ConverseStream wire whose terminal
// metadata carries cache read+write token counts.
func cacheBedrockStreamWire() []byte {
	var wire []byte
	wire = append(wire, bedrockFrame("messageStart", `{"role":"assistant"}`)...)
	jb, _ := json.Marshal("Hello")
	wire = append(wire, bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":`+string(jb)+`}}`)...)
	wire = append(wire, bedrockFrame("contentBlockStop", `{"contentBlockIndex":0}`)...)
	wire = append(wire, bedrockFrame("messageStop", `{"stopReason":"end_turn"}`)...)
	wire = append(wire, bedrockFrame("metadata",
		`{"usage":{"inputTokens":100,"outputTokens":20,"totalTokens":120,"cacheReadInputTokens":30,"cacheWriteInputTokens":20}}`)...)
	return wire
}

// splitSSEEvents splits a raw SSE body into (eventName -> dataJSON) records, in
// order, so a test can target a SPECIFIC named event (e.g. message_delta vs
// message_start). Each SSE record here is "event: <name>\ndata: <json>\n\n".
type sseRecord struct {
	event string
	data  string
}

func splitSSEEvents(t *testing.T, body string) []sseRecord {
	t.Helper()
	var recs []sseRecord
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		recs = append(recs, sseRecord{event: name, data: data})
	}
	return recs
}

// TestPumpStream_AnthropicOutput_CacheOnTerminalMessageDelta drives the PRODUCTION
// pumpStream with a Bedrock upstream carrying cache read+write metadata and asserts
// that the cache fields (and input_tokens) ride the TERMINAL message_delta — NOT
// the lazily-emitted message_start (which stays at the fixed input/output=0 usage,
// because Bedrock's usage/metadata arrives only at the end of the stream).
func TestPumpStream_AnthropicOutput_CacheOnTerminalMessageDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, usage := runPumpStreamRaw(t, OutAnthropic, cacheBedrockStreamWire())

	// Sanity: the accumulated Usage carried cache both ways (drives the render).
	if usage == nil || usage.CacheReadTokens != 30 || usage.CacheWriteTokens != 20 {
		t.Fatalf("finalUsage cache = %+v, want read 30 / write 20", usage)
	}

	recs := splitSSEEvents(t, body)
	var start, delta *sseRecord
	for i := range recs {
		switch recs[i].event {
		case "message_start":
			start = &recs[i]
		case "message_delta":
			delta = &recs[i] // last message_delta wins (there is exactly one terminal)
		}
	}
	if start == nil || delta == nil {
		t.Fatalf("missing framing: message_start=%v message_delta=%v\nbody:\n%s", start != nil, delta != nil, body)
	}

	// message_start MUST NOT carry cache fields (Bedrock timing: usage is metadata-last).
	if strings.Contains(start.data, "cache_read_input_tokens") || strings.Contains(start.data, "cache_creation_input_tokens") {
		t.Errorf("message_start must NOT carry cache fields, got: %s", start.data)
	}

	// Terminal message_delta MUST carry input + both cache buckets.
	var md struct {
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheReadTokens  int `json:"cache_read_input_tokens"`
			CacheWriteTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(delta.data), &md); err != nil {
		t.Fatalf("decode terminal message_delta: %v\ndata: %s", err, delta.data)
	}
	if md.Usage.OutputTokens != 20 {
		t.Errorf("terminal output_tokens = %d, want 20", md.Usage.OutputTokens)
	}
	if md.Usage.InputTokens != 100 {
		t.Errorf("terminal input_tokens = %d, want 100", md.Usage.InputTokens)
	}
	if md.Usage.CacheReadTokens != 30 {
		t.Errorf("terminal cache_read_input_tokens = %d, want 30", md.Usage.CacheReadTokens)
	}
	if md.Usage.CacheWriteTokens != 20 {
		t.Errorf("terminal cache_creation_input_tokens = %d, want 20", md.Usage.CacheWriteTokens)
	}
}

// TestPumpStream_AnthropicOutput_NoCacheNoKeys is the back-compat sibling: a
// Bedrock stream WITHOUT cache metadata must produce a terminal message_delta with
// no cache keys (and message_start still clean).
func TestPumpStream_AnthropicOutput_NoCacheNoKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var wire []byte
	wire = append(wire, bedrockFrame("messageStart", `{"role":"assistant"}`)...)
	jb, _ := json.Marshal("Hi")
	wire = append(wire, bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":`+string(jb)+`}}`)...)
	wire = append(wire, bedrockFrame("contentBlockStop", `{"contentBlockIndex":0}`)...)
	wire = append(wire, bedrockFrame("messageStop", `{"stopReason":"end_turn"}`)...)
	wire = append(wire, bedrockFrame("metadata", `{"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`)...)

	body, _ := runPumpStreamRaw(t, OutAnthropic, wire)
	if strings.Contains(body, "cache_read_input_tokens") || strings.Contains(body, "cache_creation_input_tokens") {
		t.Errorf("no-cache stream must NOT emit any cache field, got body:\n%s", body)
	}
}

// TestBuildResponse_AnthropicOutput_CacheFromBedrockUpstream proves the NON-STREAM
// reformat path: a Bedrock Converse JSON response with cacheRead/WriteInputTokens
// is parsed by the PRODUCTION Bedrock adapter and rendered via the PRODUCTION
// buildResponse(OutAnthropic, ...) into an Anthropic body carrying
// cache_read_input_tokens=30 / cache_creation_input_tokens=20 (mirrors
// serveNonStream: ParseResponse → buildResponse).
func TestBuildResponse_AnthropicOutput_CacheFromBedrockUpstream(t *testing.T) {
	const upstreamBody = `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},` +
		`"stopReason":"end_turn",` +
		`"usage":{"inputTokens":100,"outputTokens":20,"totalTokens":120,"cacheReadInputTokens":30,"cacheWriteInputTokens":20}}`

	ad := adapter.NewBedrockAdapter(stubDecryptorBR{})
	uniResp, usage, err := ad.ParseResponse(&http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if usage.CacheReadTokens != 30 || usage.CacheWriteTokens != 20 {
		t.Fatalf("parsed cache = read %d / write %d, want 30 / 20", usage.CacheReadTokens, usage.CacheWriteTokens)
	}
	uniResp.Usage = usage

	body, ok := buildResponse(OutAnthropic, uniResp).(map[string]any)
	if !ok {
		t.Fatalf("buildResponse did not return a map")
	}
	rendered := body["usage"].(map[string]any)
	if rendered["cache_read_input_tokens"] != 30 {
		t.Errorf("cache_read_input_tokens = %v, want 30", rendered["cache_read_input_tokens"])
	}
	if rendered["cache_creation_input_tokens"] != 20 {
		t.Errorf("cache_creation_input_tokens = %v, want 20", rendered["cache_creation_input_tokens"])
	}
}
