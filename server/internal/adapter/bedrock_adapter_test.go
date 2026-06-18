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

func TestBedrockAdapter_BuildRequest(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "bedrock-key"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}

	tests := []struct {
		name    string
		stream  bool
		wantURL string
	}{
		{"non-stream", false, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/converse"},
		{"stream", true, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/converse-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uni := UnifiedRequest{
				Model:     "anthropic.claude-3-5-sonnet-20240620-v1:0",
				System:    "you are helpful",
				Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
				Stream:    tc.stream,
				MaxTokens: intPtr(100),
			}
			req, err := a.BuildRequest(context.Background(), uni, ch)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if req.URL.String() != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL.String(), tc.wantURL)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer bedrock-key" {
				t.Errorf("Authorization = %q, want Bearer bedrock-key", got)
			}

			var body bedrockConverseRequest
			if err := json.Unmarshal(readBody(t, req), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(body.System) != 1 || body.System[0].Text != "you are helpful" {
				t.Errorf("system = %+v", body.System)
			}
			if len(body.Messages) != 1 || body.Messages[0].Role != RoleUser ||
				len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0].Text != "hi" {
				t.Errorf("messages = %+v", body.Messages)
			}
			if body.InferenceConfig == nil || body.InferenceConfig.MaxTokens == nil || *body.InferenceConfig.MaxTokens != 100 {
				t.Errorf("inferenceConfig = %+v", body.InferenceConfig)
			}
		})
	}
}

func TestApplyInferenceProfile(t *testing.T) {
	const opus = "anthropic.claude-opus-4-8"
	tests := []struct {
		name    string
		modelID string
		region  string
		enabled bool
		want    string
	}{
		{"bare anthropic + us-east-1 → us. prefix", opus, "us-east-1", true, "us." + opus},
		{"bare anthropic + eu-west-1 → eu. prefix", opus, "eu-west-1", true, "eu." + opus},
		{"bare anthropic + ap-southeast-1 → apac. prefix", opus, "ap-southeast-1", true, "apac." + opus},
		{"ca-central-1 → NO prefix (not in us. group)", opus, "ca-central-1", true, opus},
		{"us-gov-east-1 → NO prefix (GovCloud not in us. group)", opus, "us-gov-east-1", true, opus},
		{"already us. prefixed → unchanged", "us." + opus, "us-east-1", true, "us." + opus},
		{"already global. prefixed → unchanged", "global." + opus, "us-east-1", true, "global." + opus},
		{"already eu. prefixed → unchanged", "eu." + opus, "eu-west-1", true, "eu." + opus},
		{"already apac. prefixed → unchanged", "apac." + opus, "ap-southeast-1", true, "apac." + opus},
		{"flag off → unchanged", opus, "us-east-1", false, opus},
		{"empty region → unchanged", opus, "", true, opus},
		{"unknown region → unchanged", opus, "mars-north-1", true, opus},
		{"no-dot id (not provider-qualified) → unchanged", "custom-model", "us-east-1", true, "custom-model"},
		{"amazon provider id → us. prefix", "amazon.nova-pro-v1:0", "us-east-1", true, "us.amazon.nova-pro-v1:0"},
		{"meta provider id → apac. prefix", "meta.llama3-1-70b-instruct-v1:0", "ap-northeast-1", true, "apac.meta.llama3-1-70b-instruct-v1:0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyInferenceProfile(tc.modelID, tc.region, tc.enabled); got != tc.want {
				t.Errorf("applyInferenceProfile(%q, %q, %v) = %q, want %q", tc.modelID, tc.region, tc.enabled, got, tc.want)
			}
		})
	}
}

func TestRegionToProfilePrefix(t *testing.T) {
	tests := []struct {
		region string
		want   string
	}{
		{"us-east-1", "us."},
		{"us-west-2", "us."},
		{"eu-west-1", "eu."},
		{"eu-central-1", "eu."},
		{"ap-southeast-1", "apac."},
		{"ap-northeast-1", "apac."},
		{"ca-central-1", ""},
		{"us-gov-east-1", ""},
		{"us-gov-west-1", ""},
		{"", ""},
		{"unknown", ""},
	}
	for _, tc := range tests {
		if got := regionToProfilePrefix(tc.region); got != tc.want {
			t.Errorf("regionToProfilePrefix(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

// TestBedrockAdapter_BuildRequest_InferenceProfile asserts the flag toggles the
// model segment of the Converse URL: on → region-prefixed, off → bare.
func TestBedrockAdapter_BuildRequest_InferenceProfile(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	uni := UnifiedRequest{
		Model:     "anthropic.claude-opus-4-8",
		Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
		MaxTokens: intPtr(10),
	}

	tests := []struct {
		name    string
		enabled bool
		wantURL string
	}{
		{"flag on → us. prefixed", true, "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-opus-4-8/converse"},
		{"flag off → bare id", false, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-opus-4-8/converse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1", UseInferenceProfile: tc.enabled}
			req, err := a.BuildRequest(context.Background(), uni, ch)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if req.URL.String() != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL.String(), tc.wantURL)
			}
		})
	}
}

func TestBedrockAdapter_BuildRequest_MissingRegion(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock}
	if _, err := a.BuildRequest(context.Background(), UnifiedRequest{Model: "m"}, ch); err == nil {
		t.Fatal("expected error for missing region")
	}
}

func TestBedrockAdapter_ParseResponse(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	respBody := `{
		"output":{"message":{"role":"assistant","content":[{"text":"hello"},{"text":"world"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":12,"outputTokens":8,"totalTokens":20}
	}`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(respBody))}

	out, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(out.Content))
	}
	if out.Text() != "hello\nworld" {
		t.Errorf("text = %q", out.Text())
	}
	if out.StopReason != StopEndTurn {
		t.Errorf("stop = %q", out.StopReason)
	}
	if !usage.HasUpstream || usage.PromptTokens != 12 || usage.CompletionTokens != 8 || usage.TotalTokens != 20 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestBedrockAdapter_ParseResponse_Error(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	resp := &http.Response{
		StatusCode: 400,
		Status:     "400 Bad Request",
		Body:       io.NopCloser(strings.NewReader(`{"message":"invalid model"}`)),
	}
	_, _, err := a.ParseResponse(resp)
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.StatusCode != 400 || ue.Message != "invalid model" {
		t.Errorf("UpstreamError = %+v", ue)
	}
	if ue.Retryable() {
		t.Error("400 should not be retryable")
	}
}

// TestBedrockAdapter_ParseStreamChunk exercises the PRIMARY real-AWS path:
// dispatch on the :event-type header value with an UNWRAPPED inner-JSON payload
// (the shape AWS ConverseStream actually sends — the discriminator is in the
// header, not an outer JSON key).
func TestBedrockAdapter_ParseStreamChunk(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})

	// contentBlockDelta (unwrapped) → text
	c, ok, err := a.ParseStreamChunk("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"text":"Hel"}}`))
	if err != nil || !ok || c.Delta != "Hel" {
		t.Fatalf("delta: ok=%v err=%v delta=%q", ok, err, c.Delta)
	}

	// messageStop (unwrapped) → stop reason + done
	c, ok, _ = a.ParseStreamChunk("messageStop", []byte(`{"stopReason":"max_tokens"}`))
	if !ok || c.StopReason != StopMaxTokens || !c.Done {
		t.Errorf("messageStop: ok=%v stop=%q done=%v", ok, c.StopReason, c.Done)
	}

	// metadata (unwrapped) → usage + done
	c, ok, _ = a.ParseStreamChunk("metadata", []byte(`{"usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}`))
	if !ok || c.Usage == nil || c.Usage.TotalTokens != 10 || !c.Done {
		t.Errorf("metadata: ok=%v usage=%+v done=%v", ok, c.Usage, c.Done)
	}

	// messageStart → structural, not meaningful
	_, ok, _ = a.ParseStreamChunk("messageStart", []byte(`{"role":"assistant"}`))
	if ok {
		t.Error("messageStart should not be meaningful")
	}

	// contentBlockStart → structural, not meaningful
	_, ok, _ = a.ParseStreamChunk("contentBlockStart", []byte(`{"contentBlockIndex":0}`))
	if ok {
		t.Error("contentBlockStart should not be meaningful")
	}

	// contentBlockStop → structural, not meaningful
	_, ok, _ = a.ParseStreamChunk("contentBlockStop", []byte(`{"contentBlockIndex":0}`))
	if ok {
		t.Error("contentBlockStop should not be meaningful")
	}

	// Unknown / future event-type → ignored, not meaningful, no error.
	_, ok, err = a.ParseStreamChunk("somethingNew", []byte(`{"x":1}`))
	if ok || err != nil {
		t.Errorf("unknown event-type: ok=%v err=%v, want ignored", ok, err)
	}
}

// TestBedrockAdapter_ParseStreamChunk_BeforeAfterUnwrapped is the direct
// before/after proof of the root-cause fix on a REAL unwrapped payload. The
// payload AWS sends for a contentBlockDelta carries NO outer "contentBlockDelta"
// key; the discriminator is in the :event-type header. Pre-fix, ParseStreamChunk
// keyed off the outer wrapper and so produced an empty delta (the 200-empty bug);
// post-fix it dispatches on eventType and reads delta.text.
func TestBedrockAdapter_ParseStreamChunk_BeforeAfterUnwrapped(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})
	unwrappedDelta := []byte(`{"contentBlockIndex":0,"delta":{"text":"Hello"}}`)
	unwrappedMeta := []byte(`{"usage":{"inputTokens":11,"outputTokens":5,"totalTokens":16}}`)

	// BEFORE (regression guard): the legacy wrapped-key path applied to an
	// unwrapped payload (eventType=="") drops everything — exactly the bug.
	if c, ok, _ := a.ParseStreamChunk("", unwrappedDelta); ok || c.Delta != "" {
		t.Errorf("legacy wrapped parse of unwrapped delta: ok=%v delta=%q, want dropped (proves the bug)", ok, c.Delta)
	}
	if c, ok, _ := a.ParseStreamChunk("", unwrappedMeta); ok || c.Usage != nil {
		t.Errorf("legacy wrapped parse of unwrapped metadata: ok=%v usage=%+v, want dropped (proves the bug)", ok, c.Usage)
	}

	// AFTER (the fix): dispatch on the :event-type header → non-empty delta + usage.
	c, ok, err := a.ParseStreamChunk("contentBlockDelta", unwrappedDelta)
	if err != nil || !ok || c.Delta != "Hello" {
		t.Errorf("fixed parse of unwrapped delta: ok=%v err=%v delta=%q, want \"Hello\"", ok, err, c.Delta)
	}
	c, ok, err = a.ParseStreamChunk("metadata", unwrappedMeta)
	if err != nil || !ok || c.Usage == nil || c.Usage.PromptTokens != 11 || c.Usage.CompletionTokens != 5 || c.Usage.TotalTokens != 16 {
		t.Errorf("fixed parse of unwrapped metadata: ok=%v err=%v usage=%+v, want 11/5/16", ok, err, c.Usage)
	}
}

// TestBedrockAdapter_ParseStreamChunk_Exception verifies a ConverseStream error
// event (e.g. validationException) is carried on the FATAL StreamChunk.UpstreamErr
// field with meaningful=true and NO returned error — so the pump (which skips
// returned parse errors) cannot swallow it. The status maps per exception kind.
func TestBedrockAdapter_ParseStreamChunk_Exception(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})

	c, ok, err := a.ParseStreamChunk("validationException", []byte(`{"message":"model id is invalid"}`))
	if err != nil {
		t.Fatalf("exception must NOT return an error (pumps swallow errors): err=%v", err)
	}
	if !ok {
		t.Fatal("exception chunk must be meaningful so the pump acts on it")
	}
	if c.UpstreamErr == nil {
		t.Fatal("exception must set StreamChunk.UpstreamErr (fatal field)")
	}
	if c.UpstreamErr.Message != "model id is invalid" {
		t.Errorf("UpstreamErr.Message = %q, want the upstream message", c.UpstreamErr.Message)
	}
	if c.UpstreamErr.StatusCode != 400 {
		t.Errorf("validationException status = %d, want 400", c.UpstreamErr.StatusCode)
	}

	// throttlingException → 429; message falls back to the event-type if absent.
	c, ok, _ = a.ParseStreamChunk("throttlingException", []byte(`{}`))
	if !ok || c.UpstreamErr == nil || c.UpstreamErr.StatusCode != 429 {
		t.Errorf("throttlingException: ok=%v err=%+v, want status 429", ok, c.UpstreamErr)
	}
	if c.UpstreamErr != nil && c.UpstreamErr.Message != "throttlingException" {
		t.Errorf("throttling message fallback = %q, want event-type", c.UpstreamErr.Message)
	}
}

// TestBedrockAdapter_ParseStreamChunk_WrappedFallback verifies the eventType==""
// compatibility path still parses the legacy outer-wrapped shape (for any
// non-standard / pre-deframed upstream), so the fallback is not a regression.
func TestBedrockAdapter_ParseStreamChunk_WrappedFallback(t *testing.T) {
	a := NewBedrockAdapter(stubDecryptor{})

	c, ok, err := a.ParseStreamChunk("", []byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Hel"}}}`))
	if err != nil || !ok || c.Delta != "Hel" {
		t.Fatalf("wrapped fallback delta: ok=%v err=%v delta=%q", ok, err, c.Delta)
	}
	c, ok, _ = a.ParseStreamChunk("", []byte(`{"metadata":{"usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}}`))
	if !ok || c.Usage == nil || c.Usage.TotalTokens != 10 || !c.Done {
		t.Errorf("wrapped fallback metadata: ok=%v usage=%+v done=%v", ok, c.Usage, c.Done)
	}
}

// TestFor verifies the adapter factory routes by channel type.
func TestFor(t *testing.T) {
	dec := stubDecryptor{}
	if a, ok := For(&model.Channel{Type: model.ChannelOpenAI}, dec); !ok || a.Name() != "openai" {
		t.Errorf("openai routing failed: ok=%v", ok)
	}
	if a, ok := For(&model.Channel{Type: model.ChannelBedrock}, dec); !ok || a.Name() != "bedrock" {
		t.Errorf("bedrock routing failed: ok=%v", ok)
	}
	if _, ok := For(&model.Channel{Type: "weird"}, dec); ok {
		t.Error("unknown type should not route")
	}
}
