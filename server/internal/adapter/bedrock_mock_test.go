package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// bedrock_mock_test.go stands up mock Bedrock Converse upstreams (httptest) and
// drives the BedrockAdapter end-to-end for the NON-STREAMING path and the Turn-2
// request-serialization invariant. It proves:
//
//  1. Non-stream ParseResponse reads output.message.content[].text into non-empty
//     unified text and surfaces usage.{inputTokens,outputTokens,totalTokens}.
//  2. Turn-2 fix evidence: the request body the mock RECEIVES for a multi-turn
//     conversation that includes an EMPTY assistant turn contains no {text:""}
//     block and no empty content array (the §2.1 filter, exercised via the real
//     BuildRequest serialization path shared by /v1 and test-chat).
//
// The STREAMING de-frame + dispatch path is verified in package relay
// (stream_bedrock_test.go), where the mock's REAL AWS event-stream frame bytes
// are fed through the PRODUCTION readEventStream/upstreamEvents de-framer (and
// the BedrockAdapter dispatches on the extracted :event-type). That is the
// correct cross-check: the same production de-framer that runs in prod decodes
// the test's frames — not a test-private re-implementation. The unwrapped-payload
// ParseStreamChunk dispatch itself is unit-tested in bedrock_adapter_test.go.

// ---- Non-streaming Converse: mock upstream end-to-end -----------------------

// TestBedrockMock_NonStreamConverse_SurfacesText stands up a mock Converse
// upstream returning the documented shape and drives BuildRequest → HTTP client →
// ParseResponse. It confirms (non-stream path) that output.message.content[].text
// is read into non-empty unified text and that
// usage.{inputTokens,outputTokens,totalTokens} surfaces correctly.
func TestBedrockMock_NonStreamConverse_SurfacesText(t *testing.T) {
	const wantText = "Hello from Bedrock"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"output":{"message":{"role":"assistant","content":[{"text":"Hello from Bedrock"}]}},
			"usage":{"inputTokens":11,"outputTokens":5,"totalTokens":16},
			"stopReason":"end_turn"
		}`)
	}))
	defer srv.Close()

	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model:    "anthropic.claude",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}

	resp := sendViaMock(t, a, uni, ch, srv.URL)
	defer resp.Body.Close()

	out, usage, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Text() != wantText {
		t.Errorf("text = %q, want %q (non-empty text must surface)", out.Text(), wantText)
	}
	if out.StopReason != StopEndTurn {
		t.Errorf("stopReason = %q, want end_turn", out.StopReason)
	}
	if !usage.HasUpstream || usage.PromptTokens != 11 || usage.CompletionTokens != 5 || usage.TotalTokens != 16 {
		t.Errorf("usage = %+v, want upstream 11/5/16", usage)
	}
}

// TestBedrockMock_NonStreamConverse_EmptyUpstream documents the genuine
// empty-upstream case: when the upstream returns a message with NO text content,
// the parse layer is not at fault — it correctly yields empty text. Usage falls
// back to an estimate since none was reported.
func TestBedrockMock_NonStreamConverse_EmptyUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// An assistant turn with an empty content array — a genuinely empty reply.
		_, _ = io.WriteString(w, `{
			"output":{"message":{"role":"assistant","content":[]}},
			"stopReason":"end_turn"
		}`)
	}))
	defer srv.Close()

	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}}}

	resp := sendViaMock(t, a, uni, ch, srv.URL)
	defer resp.Body.Close()

	out, _, err := a.ParseResponse(resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Text() != "" {
		t.Errorf("text = %q, want empty (genuine empty upstream is NOT a parse bug)", out.Text())
	}
}

// ---- Turn-2 fix evidence: the wire body has no empty text / empty content ----

// TestBedrockMock_Turn2_NoEmptyBlocksOnWire is the §2.1 regression that proves the
// fix on the wire. It sends a multi-turn conversation that includes an EMPTY
// assistant placeholder (the exact shape produced by a Turn-1 empty reply) and a
// user turn padded with an empty text block, through the SHARED BuildRequest path
// (the same one /v1 and test-chat use). It captures the body the mock upstream
// receives and asserts: (a) no content block serializes to {text:""}, (b) no
// message has an empty content array, (c) the empty assistant message is dropped
// entirely, and (d) the real assistant/user text is preserved. This is what stops
// AWS's "ContentBlock object at messages.N.content.0 must set one of ..." error.
func TestBedrockMock_Turn2_NoEmptyBlocksOnWire(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn"}`)
	}))
	defer srv.Close()

	a := NewBedrockAdapter(stubDecryptor{key: "k"})
	ch := &model.Channel{Type: model.ChannelBedrock, Region: "us-east-1"}
	uni := UnifiedRequest{
		Model: "anthropic.claude",
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("first question")}},
			// Turn-1 produced an empty assistant reply → an empty text block.
			{Role: RoleAssistant, Content: []ContentBlock{TextBlock("")}},
			// Next user turn; a stray empty text block alongside real text.
			{Role: RoleUser, Content: []ContentBlock{TextBlock(""), TextBlock("second question")}},
		},
	}

	resp := sendViaMock(t, a, uni, ch, srv.URL)
	resp.Body.Close()

	// Decode into the wire struct and assert the invariants structurally.
	var body bedrockConverseRequest
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	// The empty assistant message must have been dropped: 2 messages remain.
	if len(body.Messages) != 2 {
		t.Fatalf("messages on wire = %d, want 2 (empty assistant dropped); body=%s", len(body.Messages), captured)
	}
	for i, m := range body.Messages {
		if len(m.Content) == 0 {
			t.Errorf("message[%d] role=%s has empty content array (Bedrock rejects this)", i, m.Role)
		}
		for j, blk := range m.Content {
			if blk.Image == nil && blk.Text == "" {
				t.Errorf("message[%d].content[%d] is an empty {text:\"\"} block (Bedrock rejects this)", i, j)
			}
		}
	}
	if body.Messages[0].Role != RoleUser || body.Messages[0].Content[0].Text != "first question" {
		t.Errorf("message[0] = %+v, want user 'first question'", body.Messages[0])
	}
	if body.Messages[1].Role != RoleUser ||
		len(body.Messages[1].Content) != 1 || body.Messages[1].Content[0].Text != "second question" {
		t.Errorf("message[1] = %+v, want single user 'second question' block", body.Messages[1])
	}

	// Belt-and-suspenders: the raw JSON must not contain an empty-text block or an
	// empty content array anywhere.
	if bytes.Contains(captured, []byte(`"text":""`)) {
		t.Errorf("wire body contains an empty text block: %s", captured)
	}
	if bytes.Contains(captured, []byte(`"content":[]`)) {
		t.Errorf("wire body contains an empty content array: %s", captured)
	}
}

// ---- shared mock-send helper ------------------------------------------------

// sendViaMock builds the upstream request via the adapter (exercising the real
// unifiedToBedrock serialization), then replays that exact body+headers against
// the mock server URL (the role the relay's http.Client plays). It returns the
// mock's *http.Response for ParseResponse. Using the real BuildRequest output is
// what makes this an end-to-end test of the adapter's serialize→wire→parse
// contract while keeping the upstream a controllable mock.
func sendViaMock(t *testing.T, a *BedrockAdapter, uni UnifiedRequest, ch *model.Channel, mockURL string) *http.Response {
	t.Helper()
	req, err := a.BuildRequest(context.Background(), uni, ch)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read built body: %v", err)
	}
	// Sanity: BuildRequest must target the real regional Converse endpoint and set
	// the bearer auth header (we then redirect the body to the mock).
	if req.URL.Host == "" || req.Header.Get("Authorization") == "" {
		t.Fatalf("BuildRequest produced an unexpected request: url=%s auth=%q", req.URL, req.Header.Get("Authorization"))
	}

	mockReq, err := http.NewRequest(http.MethodPost, mockURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new mock request: %v", err)
	}
	mockReq.Header = req.Header.Clone()
	resp, err := http.DefaultClient.Do(mockReq)
	if err != nil {
		t.Fatalf("send to mock: %v", err)
	}
	return resp
}
