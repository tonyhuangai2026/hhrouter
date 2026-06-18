package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

const tcSecret = "0123456789abcdef0123456789abcdef"

func init() { gin.SetMode(gin.TestMode) }

// newTestChatDB opens an isolated in-memory sqlite DB with the Channel table.
func newTestChatDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:tctest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// seedTCChannel inserts an OpenAI channel pointing at baseURL with the given
// model list and returns its id.
func seedTCChannel(t *testing.T, gdb *gorm.DB, baseURL string, models []string) uint {
	t.Helper()
	enc, err := crypto.Encrypt(tcSecret, "sk-upstream")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mj, _ := json.Marshal(models)
	ch := &model.Channel{
		Name:    "oa",
		Type:    model.ChannelOpenAI,
		BaseURL: baseURL,
		Key:     enc,
		Models:  mj,
		Status:  model.ChannelEnabled,
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch.ID
}

// buildTestChatRouter mounts the test-chat handler BARE (no auth middleware).
// The real JWTAuth+AdminOnly chain is exercised separately in the api package
// test (relay cannot import middleware: middleware imports relay). These tests
// focus on the handler's request/response behaviour.
func buildTestChatRouter(gdb *gorm.DB) *gin.Engine {
	channelSvc := service.NewChannelService(gdb, nil, tcSecret)
	// These functional tests exercise request/response behaviour only and do not
	// migrate request_logs, so the LogService is left nil (the is_test audit write
	// is then skipped). Logging is covered by TestTestChat_WritesIsTestLog below.
	ctrl := NewTestChatController(channelSvc, nil, nil)

	r := gin.New()
	r.POST("/api/channels/:id/test-chat", ctrl.TestChat)
	return r
}

func doTestChat(t *testing.T, r *gin.Engine, id uint, _ string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/channels/"+itoa(id)+"/test-chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// adminToken is retained as a no-op placeholder so the functional tests read
// naturally (auth is enforced by the middleware chain, verified in the api test).
func adminToken(*testing.T) string { return "" }

func itoa(u uint) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}

// --- Non-streaming happy path -----------------------------------------------

func TestTestChat_NonStream(t *testing.T) {
	gdb := newTestChatDB(t)

	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	w := doTestChat(t, r, id, adminToken(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	if gotAuth != "Bearer sk-upstream" {
		t.Fatalf("upstream auth=%q", gotAuth)
	}
	if gotBody["model"] != "gpt-4o" {
		t.Fatalf("upstream model=%v", gotBody["model"])
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object=%v", resp["object"])
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", resp["choices"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello there" {
		t.Fatalf("content=%v", msg["content"])
	}
	if _, ok := resp["usage"]; !ok {
		t.Fatalf("missing usage")
	}
}

// --- Streaming happy path ---------------------------------------------------

func TestTestChat_Stream(t *testing.T) {
	gdb := newTestChatDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`)
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
		writeChunk("[DONE]")
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	// c.Stream needs the http.CloseNotifier path, so drive the router over a real
	// TCP listener (matching the existing relay streaming e2e tests) rather than a
	// ResponseRecorder.
	front := httptest.NewServer(r)
	defer front.Close()

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/api/channels/"+itoa(id)+"/test-chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := string(raw)
	if !strings.Contains(out, `"delta":{"content":"Hel"}`) {
		t.Fatalf("missing first delta in:\n%s", out)
	}
	if !strings.Contains(out, `"delta":{"content":"lo"}`) {
		t.Fatalf("missing second delta in:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("missing trailing [DONE] in:\n%s", out)
	}
}

// seedTCChannelTyped inserts a channel of the given type (used for the anthropic
// upstream streaming-usage regression test).
func seedTCChannelTyped(t *testing.T, gdb *gorm.DB, typ model.ChannelType, baseURL string, models []string) uint {
	t.Helper()
	enc, err := crypto.Encrypt(tcSecret, "sk-upstream")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mj, _ := json.Marshal(models)
	ch := &model.Channel{Name: "an", Type: typ, BaseURL: baseURL, Key: enc, Models: mj, Status: model.ChannelEnabled}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch.ID
}

// TestTestChat_StreamAnthropicUsage is the regression test for the Playground
// "input token = 0" bug: an Anthropic upstream splits usage across two SSE
// events — message_start (input_tokens, completion=0) and message_delta
// (output_tokens, prompt=0). The pump must emit the MERGED running total on the
// streamed chunk so the client (which keeps the last usage object wholesale)
// sees BOTH a non-zero prompt and completion, not whichever single field the
// last usage-bearing event carried.
func TestTestChat_StreamAnthropicUsage(t *testing.T) {
	gdb := newTestChatDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		// Real Anthropic SSE shape: input_tokens on message_start, output_tokens
		// on message_delta.
		writeChunk(`{"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":25,"output_tokens":0}}}`)
		writeChunk(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		writeChunk(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`)
		writeChunk(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
		writeChunk(`{"type":"message_stop"}`)
	}))
	defer srv.Close()

	id := seedTCChannelTyped(t, gdb, model.ChannelAnthropic, srv.URL, []string{"claude-opus-4-8"})
	r := buildTestChatRouter(gdb)
	front := httptest.NewServer(r)
	defer front.Close()

	body := `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/api/channels/"+itoa(id)+"/test-chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := string(raw)

	// Find the LAST usage object in the streamed OpenAI chunks — this is what the
	// frontend keeps (api/stream.js: lastUsage = obj.usage). It MUST carry both a
	// non-zero prompt and completion, proving the merge reached the wire.
	last := lastStreamUsage(t, out)
	if last == nil {
		t.Fatalf("no usage object in streamed chunks:\n%s", out)
	}
	if pt, _ := last["prompt_tokens"].(float64); pt != 25 {
		t.Fatalf("streamed prompt_tokens = %v, want 25 (the input-token=0 bug)\n%s", last["prompt_tokens"], out)
	}
	if ct, _ := last["completion_tokens"].(float64); ct != 7 {
		t.Fatalf("streamed completion_tokens = %v, want 7\n%s", last["completion_tokens"], out)
	}
}

// lastStreamUsage scans an SSE body of OpenAI chat.completion.chunk lines and
// returns the usage object from the LAST chunk that carried one (mirroring the
// frontend's last-wins behavior).
func lastStreamUsage(t *testing.T, body string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			continue
		}
		if u, ok := obj["usage"].(map[string]any); ok {
			last = u
		}
	}
	return last
}

// --- TTFT (first_token_ms) capture ------------------------------------------

// TestTestChat_StreamWritesFirstTokenMs asserts that a STREAMING test-chat
// request persists a request_log row whose FirstTokenMs is non-nil (the TTFT
// was captured at the first content delta), while a NON-STREAMING request
// leaves FirstTokenMs nil (there is no first-token concept). Runs entirely on
// the in-memory LogService — no external infra needed.
func TestTestChat_StreamWritesFirstTokenMs(t *testing.T) {
	gdb := newTestChatLogDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`)
		writeChunk(`{"model":"gpt-4o","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
		writeChunk("[DONE]")
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatLogRouter(gdb, 9)
	front := httptest.NewServer(r)
	defer front.Close()

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/api/channels/"+itoa(id)+"/test-chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want exactly 1", len(rows))
	}
	got := rows[0]
	if !got.IsStream {
		t.Fatalf("IsStream = false, want true")
	}
	if got.FirstTokenMs == nil {
		t.Fatalf("FirstTokenMs = nil, want non-nil for a streamed request with content deltas")
	}
	if *got.FirstTokenMs < 0 {
		t.Fatalf("FirstTokenMs = %d, want >= 0", *got.FirstTokenMs)
	}
}

// TestTestChat_NonStreamLeavesFirstTokenNil asserts the NON-streaming path
// writes a row with FirstTokenMs nil (TTFT is a stream-only concept).
func TestTestChat_NonStreamLeavesFirstTokenNil(t *testing.T) {
	gdb := newTestChatLogDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatLogRouter(gdb, 9)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := doTestChat(t, r, id, adminToken(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want exactly 1", len(rows))
	}
	if rows[0].FirstTokenMs != nil {
		t.Fatalf("FirstTokenMs = %d, want nil for a non-streaming request", *rows[0].FirstTokenMs)
	}
}

// --- Image parts (base64 + http URL) translation ----------------------------

func TestTestChat_ImageParts(t *testing.T) {
	gdb := newTestChatDB(t)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotBody, _ = io.ReadAll(req.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	// One message with text + a base64 data URL image + an http URL image.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"describe"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}` +
		`]}]}`
	w := doTestChat(t, r, id, adminToken(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	// The upstream body must carry a parts array preserving both images.
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode upstream body: %v\n%s", err, gotBody)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("messages=%d", len(parsed.Messages))
	}
	parts := parsed.Messages[0].Content
	if len(parts) != 3 {
		t.Fatalf("parts=%d want 3: %s", len(parts), gotBody)
	}
	if parts[0].Type != "text" || parts[0].Text != "describe" {
		t.Fatalf("part0=%+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("part1 (base64)=%+v", parts[1])
	}
	if parts[2].Type != "image_url" || parts[2].ImageURL == nil || parts[2].ImageURL.URL != "https://example.com/cat.jpg" {
		t.Fatalf("part2 (http)=%+v", parts[2])
	}
}

// --- Model resolution -------------------------------------------------------

func TestTestChat_ModelResolution(t *testing.T) {
	gdb := newTestChatDB(t)

	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &b)
		gotModel, _ = b["model"].(string)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	r := buildTestChatRouter(gdb)
	tok := adminToken(t)

	// (a) Omitted model, sole channel model → uses it.
	idSingle := seedTCChannel(t, gdb, srv.URL, []string{"only-model"})
	w := doTestChat(t, r, idSingle, tok, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sole model: code=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "only-model" {
		t.Fatalf("resolved model=%q want only-model", gotModel)
	}

	// (b) Omitted model, multiple channel models → 400.
	idMulti := seedTCChannel(t, gdb, srv.URL, []string{"a", "b"})
	w = doTestChat(t, r, idMulti, tok, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multi model omitted: code=%d want 400", w.Code)
	}

	// (c) Explicit model not in the channel list → proceeds with a warning.
	w = doTestChat(t, r, idMulti, tok, `{"model":"c","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("not-in-list: code=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "c" {
		t.Fatalf("resolved model=%q want c", gotModel)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["warning"]; !ok {
		t.Fatalf("expected warning field, got %s", w.Body.String())
	}
}

// --- Upstream error mapping -------------------------------------------------

func TestTestChat_UpstreamError(t *testing.T) {
	gdb := newTestChatDB(t)

	// 4xx upstream → propagated status (here 400) with surfaced message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	w := doTestChat(t, r, id, adminToken(t), `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("4xx upstream: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode err resp: %v", err)
	}
	if !strings.Contains(resp.Error.Message, "bad model") {
		t.Fatalf("error message not surfaced: %q", resp.Error.Message)
	}
}

func TestTestChat_UpstreamUnreachable(t *testing.T) {
	gdb := newTestChatDB(t)
	// Point at a closed port → dial failure → 502.
	id := seedTCChannel(t, gdb, "http://127.0.0.1:1", []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	w := doTestChat(t, r, id, adminToken(t), `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable: code=%d want 502 body=%s", w.Code, w.Body.String())
	}
}

// --- Bad request: missing messages ------------------------------------------

func TestTestChat_BadRequest(t *testing.T) {
	gdb := newTestChatDB(t)
	id := seedTCChannel(t, gdb, "http://example.invalid", []string{"gpt-4o"})
	r := buildTestChatRouter(gdb)

	w := doTestChat(t, r, id, adminToken(t), `{"model":"gpt-4o"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing messages: code=%d want 400", w.Code)
	}

	// Malformed JSON.
	w = doTestChat(t, r, id, adminToken(t), `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed: code=%d want 400", w.Code)
	}
}

// --- is_test logging (Tech Design §3.2) -------------------------------------

// newTestChatLogDB opens an isolated sqlite DB with BOTH the Channel and the
// RequestLog tables migrated, so the LogService can persist the is_test row.
func newTestChatLogDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:tclogtest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.RequestLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// buildTestChatLogRouter mounts test-chat with a real LogService and a stub
// middleware that sets the admin uid context key JWTAuth would normally set, so
// the persisted row records a non-zero user_id.
func buildTestChatLogRouter(gdb *gorm.DB, adminUID uint) *gin.Engine {
	channelSvc := service.NewChannelService(gdb, nil, tcSecret)
	logSvc := service.NewLogService(gdb)
	ctrl := NewTestChatController(channelSvc, logSvc, nil)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("uid", adminUID)
		c.Next()
	})
	r.POST("/api/channels/:id/test-chat", ctrl.TestChat)
	return r
}

// TestTestChat_WritesIsTestLog asserts the non-streaming success path writes
// exactly one request_log row that is is_test=true, token_id NULL, carries the
// admin uid + channel/model/tokens/status, and that NO quota service is involved
// (the controller has none — it is structurally impossible to consume quota).
func TestTestChat_WritesIsTestLog(t *testing.T) {
	gdb := newTestChatLogDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer srv.Close()

	const adminUID = uint(42)
	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatLogRouter(gdb, adminUID)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	w := doTestChat(t, r, id, adminToken(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want exactly 1", len(rows))
	}
	got := rows[0]
	if !got.IsTest {
		t.Errorf("IsTest = false, want true")
	}
	if got.TokenID != nil {
		t.Errorf("TokenID = %v, want nil (test-chat is not key-scoped)", *got.TokenID)
	}
	if got.UserID != adminUID {
		t.Errorf("UserID = %d, want %d (admin uid from ctx)", got.UserID, adminUID)
	}
	if got.ChannelID != id {
		t.Errorf("ChannelID = %d, want %d", got.ChannelID, id)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", got.Model)
	}
	if got.Status != model.LogSuccess {
		t.Errorf("Status = %q, want success", got.Status)
	}
	if got.HTTPStatus != http.StatusOK {
		t.Errorf("HTTPStatus = %d, want 200", got.HTTPStatus)
	}
	if got.PromptTokens != 5 || got.CompletionTokens != 2 || got.TotalTokens != 7 {
		t.Errorf("tokens = %d/%d/%d, want 5/2/7", got.PromptTokens, got.CompletionTokens, got.TotalTokens)
	}
	if got.IsStream {
		t.Errorf("IsStream = true, want false")
	}
	if got.InboundFormat != model.InboundOpenAI {
		t.Errorf("InboundFormat = %q, want openai", got.InboundFormat)
	}

	// token_id must be SQL NULL (not 0): a *uint nil serialises to NULL.
	var nullCount int64
	if err := gdb.Model(&model.RequestLog{}).Where("token_id IS NULL").Count(&nullCount).Error; err != nil {
		t.Fatalf("count null token_id: %v", err)
	}
	if nullCount != 1 {
		t.Errorf("rows with token_id IS NULL = %d, want 1", nullCount)
	}
}

// TestTestChat_LogCapturesUpstreamError asserts an upstream non-2xx error is
// persisted as an is_test row with status=error and the upstream error text in
// error_message (Tech Design §3.2: upstream error text must be visible).
func TestTestChat_LogCapturesUpstreamError(t *testing.T) {
	gdb := newTestChatLogDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded for test"}}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	r := buildTestChatLogRouter(gdb, 7)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := doTestChat(t, r, id, adminToken(t), body)
	if w.Code < 400 {
		t.Fatalf("expected error status, got %d", w.Code)
	}

	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want exactly 1", len(rows))
	}
	got := rows[0]
	if !got.IsTest {
		t.Errorf("IsTest = false, want true")
	}
	if got.TokenID != nil {
		t.Errorf("TokenID = %v, want nil", *got.TokenID)
	}
	if got.Status != model.LogError {
		t.Errorf("Status = %q, want error", got.Status)
	}
	if got.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429", got.HTTPStatus)
	}
	if !strings.Contains(got.ErrorMessage, "rate limit exceeded for test") {
		t.Errorf("ErrorMessage = %q, want it to contain the upstream error text", got.ErrorMessage)
	}
}

// --- test-chat USD price gate + cost (Tech Design §4.4) ---------------------

// newTestChatPriceDB migrates Channel + RequestLog + ModelPrice so the pricing
// gate and cost path can be exercised.
func newTestChatPriceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:tcpricetest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.RequestLog{}, &model.ModelPrice{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// buildTestChatPriceRouter mounts test-chat WITH a wired PricingService so the
// gate is active.
func buildTestChatPriceRouter(gdb *gorm.DB, adminUID uint) *gin.Engine {
	channelSvc := service.NewChannelService(gdb, nil, tcSecret)
	logSvc := service.NewLogService(gdb)
	pricingSvc := service.NewPricingService(gdb)
	ctrl := NewTestChatController(channelSvc, logSvc, pricingSvc)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("uid", adminUID); c.Next() })
	r.POST("/api/channels/:id/test-chat", ctrl.TestChat)
	return r
}

// TestTestChat_NoPriceRejected: with the gate active, an unpriced model is
// rejected 400 and NO upstream call is made (the mock must not be hit).
func TestTestChat_NoPriceRejected(t *testing.T) {
	gdb := newTestChatPriceDB(t)
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"}) // NO price row seeded
	r := buildTestChatPriceRouter(gdb, 5)

	w := doTestChat(t, r, id, adminToken(t), `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no-price code = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	if hit {
		t.Error("upstream was called despite missing price (gate must short-circuit)")
	}
	var rows []model.RequestLog
	gdb.Find(&rows)
	if len(rows) != 1 || rows[0].Status != model.LogError || rows[0].CostMicroUSD != nil {
		t.Fatalf("expected 1 error log with NULL cost, got %+v", rows)
	}
}

// TestTestChat_PricedCostLoggedNoQuota: a priced model succeeds, the is_test log
// records cost_micro_usd (and cache columns), and — critically — NO quota is
// consumed (test-chat has no QuotaService; cost is informational only).
func TestTestChat_PricedCostLoggedNoQuota(t *testing.T) {
	gdb := newTestChatPriceDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`))
	}))
	defer srv.Close()

	id := seedTCChannel(t, gdb, srv.URL, []string{"gpt-4o"})
	// $3/1M in, $15/1M out → cost = ceil((1000*3_000_000 + 500*15_000_000)/1e6)
	// = ceil((3_000_000_000 + 7_500_000_000)/1e6) = 10500 micro-USD.
	if err := gdb.Create(&model.ModelPrice{ChannelID: id, Model: "gpt-4o", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000}).Error; err != nil {
		t.Fatalf("seed price: %v", err)
	}
	r := buildTestChatPriceRouter(gdb, 5)

	w := doTestChat(t, r, id, adminToken(t), `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("priced code = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	var rows []model.RequestLog
	gdb.Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.CostMicroUSD == nil || *got.CostMicroUSD != 10500 {
		t.Errorf("cost = %v, want 10500 micro-USD", got.CostMicroUSD)
	}
	if got.CacheReadTokens == nil || got.CacheWriteTokens == nil {
		t.Errorf("cache columns should be set (0) on a priced success, got read=%v write=%v", got.CacheReadTokens, got.CacheWriteTokens)
	}
	if !got.IsTest || got.TokenID != nil {
		t.Errorf("must be is_test with NULL token_id, got isTest=%v tokenID=%v", got.IsTest, got.TokenID)
	}
	// test-chat never consumes quota: structurally there is no QuotaService on the
	// controller, so a non-nil cost on the log with no quota wiring proves (c).
}
