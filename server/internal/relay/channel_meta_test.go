package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/model"
)

// TestChannelInfoMap verifies channelInfoMap returns the {channel_id,
// channel_name, channel_type} shape with a RAW UTF-8 name, and returns nil for a
// nil channel so callers can inject it unconditionally.
func TestChannelInfoMap(t *testing.T) {
	if got := channelInfoMap(nil); got != nil {
		t.Fatalf("channelInfoMap(nil) = %#v, want nil", got)
	}

	ch := &model.Channel{ID: 42, Name: "上游渠道-A", Type: model.ChannelOpenAI}
	m := channelInfoMap(ch)
	if m == nil {
		t.Fatal("channelInfoMap(ch) = nil, want map")
	}
	if m["channel_id"] != ch.ID {
		t.Fatalf("channel_id = %#v, want %d", m["channel_id"], ch.ID)
	}
	// Name is the RAW UTF-8 value (JSON handles Unicode natively — no encoding).
	if m["channel_name"] != "上游渠道-A" {
		t.Fatalf("channel_name = %#v, want raw UTF-8 %q", m["channel_name"], "上游渠道-A")
	}
	if m["channel_type"] != string(model.ChannelOpenAI) {
		t.Fatalf("channel_type = %#v, want %q", m["channel_type"], string(model.ChannelOpenAI))
	}
	// Exactly the three documented keys, nothing extra.
	if len(m) != 3 {
		t.Fatalf("channelInfoMap keys = %d (%v), want exactly 3", len(m), m)
	}
}

// newTestContext returns a gin.Context backed by an httptest.ResponseRecorder so
// header assertions can read what setChannelHeaders wrote.
func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return c, rec
}

// TestSetChannelHeaders_WritesThreeHeaders asserts the three X-Router-Channel-*
// headers are written with the correct values for an ASCII channel name.
func TestSetChannelHeaders_WritesThreeHeaders(t *testing.T) {
	c, rec := newTestContext()
	ch := &model.Channel{ID: 7, Name: "mock-openai", Type: model.ChannelOpenAI}

	setChannelHeaders(c, ch)

	if got := rec.Header().Get(headerChannelID); got != "7" {
		t.Fatalf("%s = %q, want %q", headerChannelID, got, "7")
	}
	if got := rec.Header().Get(headerChannelName); got != "mock-openai" {
		t.Fatalf("%s = %q, want %q", headerChannelName, got, "mock-openai")
	}
	if got := rec.Header().Get(headerChannelType); got != string(model.ChannelOpenAI) {
		t.Fatalf("%s = %q, want %q", headerChannelType, got, string(model.ChannelOpenAI))
	}
}

// TestSetChannelHeaders_NilNoop asserts a nil channel writes NO headers at all.
func TestSetChannelHeaders_NilNoop(t *testing.T) {
	c, rec := newTestContext()

	setChannelHeaders(c, nil)

	for _, h := range []string{headerChannelID, headerChannelName, headerChannelType} {
		if got := rec.Header().Get(h); got != "" {
			t.Fatalf("ch==nil wrote header %s = %q, want empty (no-op)", h, got)
		}
	}
}

// TestSetChannelHeaders_NonASCIINamePercentEncoded asserts a channel name with
// non-ASCII (Chinese) characters is percent-encoded in the header value so the
// value contains no raw non-ASCII bytes (HTTP header values should be ASCII).
func TestSetChannelHeaders_NonASCIINamePercentEncoded(t *testing.T) {
	c, rec := newTestContext()
	const name = "上游渠道" // 4 CJK chars, 12 UTF-8 bytes, all non-ASCII
	ch := &model.Channel{ID: 9, Name: name, Type: model.ChannelAnthropic}

	setChannelHeaders(c, ch)

	got := rec.Header().Get(headerChannelName)
	if got == name {
		t.Fatalf("%s = %q was NOT encoded; a non-ASCII name must be percent-encoded", headerChannelName, got)
	}
	// The written value must be pure ASCII: no byte >= 0x80.
	for i := 0; i < len(got); i++ {
		if got[i] >= 0x80 {
			t.Fatalf("%s = %q contains a raw non-ASCII byte 0x%02x at %d", headerChannelName, got, got[i], i)
		}
	}
	// Every non-ASCII UTF-8 byte of the name must appear as an uppercase %XX escape.
	// "上游渠道" -> E4B88A E6B8B8 E6B8A0 E98193.
	want := "%E4%B8%8A%E6%B8%B8%E6%B8%A0%E9%81%93"
	if got != want {
		t.Fatalf("%s = %q, want percent-encoded %q", headerChannelName, got, want)
	}
	// Id and Type stay ASCII/verbatim.
	if rec.Header().Get(headerChannelID) != "9" {
		t.Fatalf("%s = %q, want %q", headerChannelID, rec.Header().Get(headerChannelID), "9")
	}
	if rec.Header().Get(headerChannelType) != string(model.ChannelAnthropic) {
		t.Fatalf("%s = %q, want %q", headerChannelType, rec.Header().Get(headerChannelType), string(model.ChannelAnthropic))
	}
}

// TestNonStreamBodyInjection mirrors the serveNonStream success-path injection:
// given a map[string]any response body and the serving channel, injecting
// body["router"]=channelInfoMap(ch) must add a router object carrying the RAW
// UTF-8 channel_name while leaving all pre-existing fields untouched.
func TestNonStreamBodyInjection(t *testing.T) {
	ch := &model.Channel{ID: 3, Name: "渠道-混合Mix", Type: model.ChannelOpenAI}

	// A representative non-streaming OpenAI-shaped body.
	body := map[string]any{
		"id":     "chatcmpl-xyz",
		"model":  "gpt-4o",
		"object": "chat.completion",
		"choices": []any{
			map[string]any{"index": 0, "finish_reason": "stop"},
		},
	}

	// This is exactly the injection performed in serveNonStream before c.JSON.
	body["router"] = channelInfoMap(ch)

	router, ok := body["router"].(map[string]any)
	if !ok {
		t.Fatalf("body[router] = %#v, want map[string]any", body["router"])
	}
	if router["channel_name"] != "渠道-混合Mix" {
		t.Fatalf("router.channel_name = %#v, want raw UTF-8 %q", router["channel_name"], "渠道-混合Mix")
	}
	if router["channel_id"] != ch.ID {
		t.Fatalf("router.channel_id = %#v, want %d", router["channel_id"], ch.ID)
	}
	if router["channel_type"] != string(model.ChannelOpenAI) {
		t.Fatalf("router.channel_type = %#v, want %q", router["channel_type"], string(model.ChannelOpenAI))
	}

	// Pre-existing fields are unchanged.
	if body["id"] != "chatcmpl-xyz" || body["model"] != "gpt-4o" || body["object"] != "chat.completion" {
		t.Fatalf("existing scalar fields mutated: %#v", body)
	}
	if choices, ok := body["choices"].([]any); !ok || len(choices) != 1 {
		t.Fatalf("choices field mutated: %#v", body["choices"])
	}
}
