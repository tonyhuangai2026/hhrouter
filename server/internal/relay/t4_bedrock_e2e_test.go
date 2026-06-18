//go:build e2e

// T4 end-to-end verification (build tag `e2e`) — Tech Design §5.
//
// This file is the live, postgres+redis-backed gate for the Bedrock conversation
// fixes (T1) and the test-chat is_test logging (T2), driven through the real
// TestChatController HTTP handler. It is deliberately in `package relay` (not
// relay_test) so it can inject the controller's unexported httpDo/streamDo
// transport hooks to redirect the (otherwise hardcoded) AWS Converse endpoint at
// a local MOCK Bedrock upstream — the pragmatic way to exercise the full
// serialize→wire→parse→log path without /etc/hosts + a TLS man-in-the-middle.
//
// What it proves LIVE against ephemeral docker postgres:16 + redis:7:
//
//	(1a) Turn-1 NON-STREAM: a Bedrock mock returning text yields a NON-empty reply.
//	(1b) Turn-1 STREAM:     a Bedrock ConverseStream mock (real event-stream frames)
//	     yields a NON-empty accumulated reply.
//	(2)  Turn-2 MULTI-TURN: a request whose history contains an EMPTY assistant turn
//	     (the user's exact bug) is serialized by the SHARED unifiedToBedrock path so
//	     the body the mock RECEIVES has NO {text:""} block and NO empty content
//	     array — the empty assistant message is dropped entirely. (Backend filter
//	     verified independently here, at the relay/test-chat level.)
//	(3)  is_test logging: each test-chat writes exactly one request_logs row with
//	     is_test=true and token_id NULL into REAL postgres, and the Redis quota:*
//	     counters are NEVER created/incremented (test-chat consumes no quota).
//	(4)  UPGRADE PATH: seed the OLD schema (request_logs.token_id NOT NULL), run the
//	     real Migrate()/postMigrate fixup, then confirm a token_id=NULL row inserts
//	     and persists (the Postgres-only DROP NOT NULL path SQLite cannot exercise).
//
// Run with (DSN/addr default to the ephemeral t4 docker containers on the host):
//
//	go test -tags e2e -run TestT4 ./internal/relay -v
package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/db"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

func t4Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const t4Secret = "0123456789abcdef0123456789abcdef" // 32 bytes AES-256

// t4Setup connects to the ephemeral postgres+redis, migrates, truncates, flushes
// Redis, and returns the live handles.
func t4Setup(t *testing.T) (*gorm.DB, *redis.Client, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dsn := t4Env("E2E_DB_DSN", "host=127.0.0.1 port=55432 user=postgres password=postgres dbname=t4test sslmode=disable")
	redisAddr := t4Env("E2E_REDIS_ADDR", "127.0.0.1:56379")

	gdb, err := db.Connect(dsn)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	rdb, err := db.ConnectRedis(redisAddr, "", 0)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb.FlushDB(context.Background())
	for _, tbl := range []string{"request_logs", "tokens", "channels", "routing_rules", "users", "options"} {
		gdb.Exec("TRUNCATE TABLE " + tbl + " RESTART IDENTITY CASCADE")
	}
	cleanup := func() { _ = rdb.Close() }
	return gdb, rdb, cleanup
}

// seedBedrockChannel inserts a Bedrock channel (region us-east-1) and returns id.
func seedBedrockChannel(t *testing.T, gdb *gorm.DB, rdb *redis.Client, models []string) uint {
	t.Helper()
	chSvc := service.NewChannelService(gdb, rdb, t4Secret)
	name := "mock-bedrock"
	typ := model.ChannelBedrock
	region := "us-east-1"
	key := "bedrock-bearer-key"
	if _, err := chSvc.Create(service.ChannelInput{
		Name: &name, Type: &typ, Region: &region, Key: &key, Models: &models,
	}); err != nil {
		t.Fatalf("seed bedrock channel: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("name = ?", name).First(&ch).Error; err != nil {
		t.Fatalf("load seeded channel: %v", err)
	}
	return ch.ID
}

// newBedrockTestChatCtrl builds a TestChatController whose httpDo/streamDo point
// at the mock Bedrock server `mockURL`. The adapter still BUILDS the real AWS
// Converse request (URL + bearer header + unifiedToBedrock body); we only rewrite
// the destination host to the mock just before the round-trip — so the exact body
// produced by the shared serialization path is what the mock receives.
func newBedrockTestChatCtrl(gdb *gorm.DB, mockURL string) *TestChatController {
	chSvc := service.NewChannelService(gdb, nil, t4Secret)
	logSvc := service.NewLogService(gdb)
	ctrl := NewTestChatController(chSvc, logSvc, nil)

	redirect := func(req *http.Request) (*http.Response, error) {
		mu, _ := http.NewRequest(req.Method, mockURL+req.URL.Path, req.Body)
		mu.Header = req.Header.Clone()
		return http.DefaultClient.Do(mu)
	}
	ctrl.httpDo = redirect
	ctrl.streamDo = redirect
	return ctrl
}

// mountBedrockTestChat mounts the controller with a stub middleware that sets the
// admin uid context key JWTAuth normally sets, over a real TCP listener (c.Stream
// needs the flushing path), returning the front-end server + channel-id helper.
func mountBedrockTestChat(ctrl *TestChatController, adminUID uint) *httptest.Server {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("uid", adminUID); c.Next() })
	r.POST("/api/channels/:id/test-chat", ctrl.TestChat)
	return httptest.NewServer(r)
}

func t4Post(t *testing.T, frontURL string, id uint, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(frontURL+"/api/channels/"+itoa(id)+"/test-chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post test-chat: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

// ---- AWS event-stream framing (real binary wire, mirrors stream_bedrock_test.go) ----
//
// t4Frame encodes ONE real AWS vnd.amazon.eventstream message exactly as Bedrock
// ConverseStream emits it: the event discriminator lives ONLY in the :event-type
// string header (real AWS binary header layout) and `payload` is the UNWRAPPED
// inner JSON of the event (NOT wrapped under an outer "<eventType>" key). The
// frame is:
//
//	[prelude: totalLen(4) | headersLen(4) | preludeCRC32(4)]
//	[headers ...][payload ...][messageCRC32(4)]
//
// Each header: nameLen(1) | name | valueType(1) | (string=7: valueLen(2) | value).
// These exact bytes are fed through the PRODUCTION readEventStream/upstreamEvents
// de-framer (via testchat.go's pumpOpenAIStream) — there is NO test-private
// de-framer here, so the test's encoding and prod's decoding cross-check the same
// :event-type extraction that runs in production.
func t4Frame(eventType string, payload string) []byte {
	hs := map[string]string{":event-type": eventType, ":content-type": "application/json", ":message-type": "event"}
	var headers []byte
	for name, val := range hs {
		headers = append(headers, byte(len(name)))
		headers = append(headers, name...)
		headers = append(headers, 7)
		var vl [2]byte
		binary.BigEndian.PutUint16(vl[:], uint16(len(val)))
		headers = append(headers, vl[:]...)
		headers = append(headers, val...)
	}
	p := []byte(payload)
	totalLen := 4 + 4 + 4 + len(headers) + len(p) + 4
	buf := make([]byte, 0, totalLen)
	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	buf = append(buf, prelude[:]...)
	var preludeCRC [4]byte
	binary.BigEndian.PutUint32(preludeCRC[:], crc32.ChecksumIEEE(prelude[:]))
	buf = append(buf, preludeCRC[:]...)
	buf = append(buf, headers...)
	buf = append(buf, p...)
	var msgCRC [4]byte
	binary.BigEndian.PutUint32(msgCRC[:], crc32.ChecksumIEEE(buf))
	buf = append(buf, msgCRC[:]...)
	return buf
}

// quotaKeysCount returns how many quota:* keys exist in Redis (must be 0 after
// test-chat — it never consumes quota).
func quotaKeysCount(t *testing.T, rdb *redis.Client) int {
	t.Helper()
	keys, err := rdb.Keys(context.Background(), "quota:*").Result()
	if err != nil {
		t.Fatalf("redis KEYS quota:*: %v", err)
	}
	return len(keys)
}

// =========================================================================
// (1a) Turn-1 NON-STREAM: mock returns text → non-empty reply + is_test log.
// =========================================================================

func TestT4_Bedrock_NonStream_Turn1NonEmpty_LogsIsTest_NoQuota(t *testing.T) {
	gdb, rdb, cleanup := t4Setup(t)
	defer cleanup()

	const wantText = "Hello from Bedrock Converse"
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"`+wantText+`"}]}},"usage":{"inputTokens":12,"outputTokens":6,"totalTokens":18},"stopReason":"end_turn"}`)
	}))
	defer srv.Close()

	id := seedBedrockChannel(t, gdb, rdb, []string{"anthropic.claude-3-5-sonnet-20240620-v1:0"})
	ctrl := newBedrockTestChatCtrl(gdb, srv.URL)
	front := mountBedrockTestChat(ctrl, 42)
	defer front.Close()

	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	resp, raw := t4Post(t, front.URL, id, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	// Turn-1: assistant reply must be NON-empty.
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode resp: %v\n%s", err, raw)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", out["choices"])
	}
	content, _ := choices[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != wantText {
		t.Fatalf("Turn-1 reply content=%q, want %q (NON-empty)", content, wantText)
	}

	// The Converse endpoint path must have been hit (proves real BuildRequest URL).
	if !strings.Contains(string(capturedBody), `"messages"`) {
		t.Fatalf("mock did not receive a Converse body: %s", capturedBody)
	}

	// (3) Exactly one is_test row, token_id NULL, into REAL postgres.
	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows=%d, want 1", len(rows))
	}
	g := rows[0]
	if !g.IsTest || g.TokenID != nil || g.UserID != 42 || g.ChannelID != id {
		t.Fatalf("log row mismatch: is_test=%v token_id=%v uid=%d ch=%d", g.IsTest, g.TokenID, g.UserID, g.ChannelID)
	}
	if g.Status != model.LogSuccess || g.HTTPStatus != 200 || g.IsStream {
		t.Fatalf("log outcome mismatch: status=%q http=%d stream=%v", g.Status, g.HTTPStatus, g.IsStream)
	}
	if g.PromptTokens != 12 || g.CompletionTokens != 6 || g.TotalTokens != 18 {
		t.Fatalf("log usage=%d/%d/%d, want 12/6/18", g.PromptTokens, g.CompletionTokens, g.TotalTokens)
	}
	// token_id IS NULL in SQL (not 0).
	var nullCount int64
	gdb.Model(&model.RequestLog{}).Where("token_id IS NULL").Count(&nullCount)
	if nullCount != 1 {
		t.Fatalf("token_id IS NULL count=%d, want 1", nullCount)
	}
	// (3) quota:* counters NEVER created.
	if n := quotaKeysCount(t, rdb); n != 0 {
		keys, _ := rdb.Keys(context.Background(), "quota:*").Result()
		t.Fatalf("quota:* keys=%d (%v), want 0 — test-chat must NOT consume quota", n, keys)
	}
	t.Logf("Turn-1 NON-STREAM: reply %q non-empty; 1 is_test row token_id NULL usage 12/6/18; quota:* keys=0", wantText)
}

// =========================================================================
// (1b) Turn-1 STREAM: ConverseStream mock → non-empty accumulated reply.
// =========================================================================

func TestT4_Bedrock_Stream_Turn1NonEmpty_LogsIsTest_NoQuota(t *testing.T) {
	gdb, rdb, cleanup := t4Setup(t)
	defer cleanup()

	deltas := []string{"Hel", "lo, ", "stream!"}
	const wantText = "Hello, stream!"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		fl, _ := w.(http.Flusher)
		write := func(b []byte) {
			_, _ = w.Write(b)
			if fl != nil {
				fl.Flush()
			}
		}
		// Real AWS ConverseStream wire shape: discriminator ONLY in the :event-type
		// header, payload is the UNWRAPPED inner JSON (no outer "<eventType>" key).
		write(t4Frame("messageStart", `{"role":"assistant"}`))
		for _, d := range deltas {
			jb, _ := json.Marshal(d)
			write(t4Frame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":`+string(jb)+`}}`))
		}
		write(t4Frame("contentBlockStop", `{"contentBlockIndex":0}`))
		write(t4Frame("messageStop", `{"stopReason":"end_turn"}`))
		write(t4Frame("metadata", `{"usage":{"inputTokens":8,"outputTokens":4,"totalTokens":12}}`))
	}))
	defer srv.Close()

	id := seedBedrockChannel(t, gdb, rdb, []string{"anthropic.claude-3-5-sonnet-20240620-v1:0"})
	ctrl := newBedrockTestChatCtrl(gdb, srv.URL)
	front := mountBedrockTestChat(ctrl, 7)
	defer front.Close()

	body := `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, raw := t4Post(t, front.URL, id, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q, want text/event-stream", ct)
	}

	// Accumulate the OpenAI-dialect SSE deltas the test-chat re-emits.
	var acc strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
			acc.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if acc.String() != wantText {
		t.Fatalf("Turn-1 STREAM accumulated=%q, want %q (NON-empty)", acc.String(), wantText)
	}
	if !strings.HasSuffix(strings.TrimSpace(raw), "data: [DONE]") {
		t.Fatalf("stream missing trailing [DONE]:\n%s", raw)
	}

	// (3) is_test row with stream usage from metadata; quota untouched.
	var rows []model.RequestLog
	gdb.Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("request_logs rows=%d, want 1", len(rows))
	}
	g := rows[0]
	if !g.IsTest || g.TokenID != nil || !g.IsStream || g.Status != model.LogSuccess {
		t.Fatalf("stream log mismatch: is_test=%v token_id=%v stream=%v status=%q", g.IsTest, g.TokenID, g.IsStream, g.Status)
	}
	if g.CompletionTokens != 4 || g.TotalTokens != 12 {
		t.Fatalf("stream log usage completion/total=%d/%d, want 4/12 (from metadata)", g.CompletionTokens, g.TotalTokens)
	}
	if n := quotaKeysCount(t, rdb); n != 0 {
		t.Fatalf("quota:* keys=%d, want 0", n)
	}
	if g.PromptTokens != 8 {
		t.Fatalf("stream log prompt tokens=%d, want 8 (from metadata.usage.inputTokens — token count must be NON-ZERO)", g.PromptTokens)
	}

	// --- MULTI-TURN: fire a SECOND streamed turn whose history carries the
	// assistant's turn-1 reply. It must also reconstruct non-empty text and write
	// a second is_test row with NON-ZERO usage — proving the de-framing fix holds
	// across consecutive turns (the user's "每轮都本轮无输出" symptom). ---------
	body2 := `{"stream":true,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"` + wantText + `"},` +
		`{"role":"user","content":"again"}` +
		`]}`
	resp2, raw2 := t4Post(t, front.URL, id, body2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Turn-2 stream status=%d body=%s", resp2.StatusCode, raw2)
	}
	var acc2 strings.Builder
	for _, line := range strings.Split(raw2, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
			acc2.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if acc2.String() != wantText {
		t.Fatalf("Turn-2 STREAM accumulated=%q, want %q (multi-turn must keep reconstructing non-empty text)", acc2.String(), wantText)
	}
	// Two is_test rows now; the latest (turn-2) must also carry non-zero usage.
	var rows2 []model.RequestLog
	gdb.Order("id desc").Find(&rows2)
	if len(rows2) != 2 {
		t.Fatalf("request_logs rows=%d after 2 turns, want 2", len(rows2))
	}
	g2 := rows2[0]
	if !g2.IsTest || g2.TokenID != nil || !g2.IsStream || g2.Status != model.LogSuccess {
		t.Fatalf("turn-2 stream log mismatch: is_test=%v token_id=%v stream=%v status=%q", g2.IsTest, g2.TokenID, g2.IsStream, g2.Status)
	}
	if g2.CompletionTokens != 4 || g2.TotalTokens != 12 || g2.PromptTokens != 8 {
		t.Fatalf("turn-2 stream usage=%d/%d/%d, want 8/4/12 (NON-ZERO from metadata)", g2.PromptTokens, g2.CompletionTokens, g2.TotalTokens)
	}
	if n := quotaKeysCount(t, rdb); n != 0 {
		t.Fatalf("quota:* keys=%d after multi-turn, want 0", n)
	}
	t.Logf("STREAM multi-turn: turn-1 + turn-2 each accumulated %q non-empty; 2 is_test rows token_id NULL usage 8/4/12 (NON-ZERO from metadata.usage); quota:* keys=0", wantText)
}

// =========================================================================
// (1c) EXCEPTION EVENT: a ConverseStream that emits :event-type=validationException
// with a {"message":...} body must surface as a FATAL stream error through the
// real test-chat path (testchat.go pumpOpenAIStream) — B2. The single is_test row
// must record status=error with the upstream message in error_message and the
// mapped http_status=400, NOT a clean 200-empty success. This exercises B2 end to
// end against REAL postgres through the production de-framer (the exception
// discriminator is carried ONLY in the :event-type header).
// =========================================================================

func TestT4_Bedrock_Stream_ExceptionEvent_StatusErrorVisible(t *testing.T) {
	gdb, rdb, cleanup := t4Setup(t)
	defer cleanup()

	const exMsg = "The provided model identifier is invalid."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		fl, _ := w.(http.Flusher)
		write := func(b []byte) {
			_, _ = w.Write(b)
			if fl != nil {
				fl.Flush()
			}
		}
		// A normal opening event, then a fatal exception mid-stream — real AWS
		// shape: discriminator in :event-type header, body is the unwrapped
		// {message}. The pre-B2 pump swallowed parse errors → clean 200-empty.
		write(t4Frame("messageStart", `{"role":"assistant"}`))
		jb, _ := json.Marshal(exMsg)
		write(t4Frame("validationException", `{"message":`+string(jb)+`}`))
	}))
	defer srv.Close()

	id := seedBedrockChannel(t, gdb, rdb, []string{"anthropic.claude-3-5-sonnet-20240620-v1:0"})
	ctrl := newBedrockTestChatCtrl(gdb, srv.URL)
	front := mountBedrockTestChat(ctrl, 5)
	defer front.Close()

	body := `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, raw := t4Post(t, front.URL, id, body)
	// Headers are committed to 200 before the exception arrives (SSE already
	// flushed), so the HTTP status to the client is 200 — the ERROR is recorded in
	// the is_test log row, which is what the dashboard surfaces.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s (SSE headers commit 200 before the mid-stream exception)", resp.StatusCode, raw)
	}

	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows=%d, want 1; stream body=%s", len(rows), raw)
	}
	g := rows[0]
	if g.Status != model.LogError {
		t.Fatalf("log Status=%q, want error — the exception must NOT be swallowed into a 200-empty success row. body=%s", g.Status, raw)
	}
	if !strings.Contains(g.ErrorMessage, exMsg) {
		t.Fatalf("log ErrorMessage=%q, want it to contain the upstream exception message %q", g.ErrorMessage, exMsg)
	}
	if g.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("log HTTPStatus=%d, want 400 (validationException maps to 400)", g.HTTPStatus)
	}
	if !g.IsStream || !g.IsTest || g.TokenID != nil {
		t.Fatalf("log flags: IsStream=%v IsTest=%v TokenID=%v, want true/true/nil", g.IsStream, g.IsTest, g.TokenID)
	}
	// Even an errored test-chat must not consume quota.
	if n := quotaKeysCount(t, rdb); n != 0 {
		t.Fatalf("quota:* keys=%d on exception path, want 0 (test-chat never consumes quota)", n)
	}
	t.Logf("STREAM EXCEPTION: validationException → is_test row status=error, http=400, error_message=%q (NOT 200-empty); quota:* keys=0", g.ErrorMessage)
}

// =========================================================================
// (2) Turn-2 MULTI-TURN: history with an EMPTY assistant turn must NOT put a
// {text:""} block or empty content array on the wire. Backend filter verified
// independently here at the relay/test-chat level (the user's exact bug).
// =========================================================================

func TestT4_Bedrock_Turn2_NoEmptyBlocksOnWire(t *testing.T) {
	gdb, rdb, cleanup := t4Setup(t)
	defer cleanup()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"second answer"}]}},"usage":{"inputTokens":20,"outputTokens":3,"totalTokens":23},"stopReason":"end_turn"}`)
	}))
	defer srv.Close()

	id := seedBedrockChannel(t, gdb, rdb, []string{"anthropic.claude-3-5-sonnet-20240620-v1:0"})
	ctrl := newBedrockTestChatCtrl(gdb, srv.URL)
	front := mountBedrockTestChat(ctrl, 1)
	defer front.Close()

	// Turn-2 send: history = [user q1, EMPTY assistant (turn-1 produced no text),
	// user q2 with a stray empty text part]. This is the exact payload that used
	// to make AWS reject "ContentBlock object at messages.1.content.0 must set
	// one of ...". The frontend empty-history filter (T3) would also drop the
	// empty assistant, but here we send it RAW to prove the BACKEND filter alone
	// removes it.
	body := `{"messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":""},` +
		`{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"second question"}]}` +
		`]}`
	resp, raw := t4Post(t, front.URL, id, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Turn-2 status=%d body=%s (must NOT error with ContentBlock empty-block)", resp.StatusCode, raw)
	}

	// Structural assertions on the body the mock RECEIVED.
	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text  string         `json:"text"`
				Image map[string]any `json:"image"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &wire); err != nil {
		t.Fatalf("unmarshal captured Converse body: %v\n%s", err, captured)
	}
	// The empty assistant message must be DROPPED → 2 messages remain.
	if len(wire.Messages) != 2 {
		t.Fatalf("wire messages=%d, want 2 (empty assistant dropped). body=%s", len(wire.Messages), captured)
	}
	for i, m := range wire.Messages {
		if len(m.Content) == 0 {
			t.Fatalf("wire message[%d] role=%s has EMPTY content array — Bedrock rejects this. body=%s", i, m.Role, captured)
		}
		for j, blk := range m.Content {
			if blk.Image == nil && blk.Text == "" {
				t.Fatalf("wire message[%d].content[%d] is an EMPTY {text:\"\"} block — the user's exact bug. body=%s", i, j, captured)
			}
		}
	}
	if wire.Messages[0].Role != "user" || wire.Messages[0].Content[0].Text != "first question" {
		t.Fatalf("wire message[0]=%+v, want user 'first question'", wire.Messages[0])
	}
	if wire.Messages[1].Role != "user" || len(wire.Messages[1].Content) != 1 || wire.Messages[1].Content[0].Text != "second question" {
		t.Fatalf("wire message[1]=%+v, want single user 'second question'", wire.Messages[1])
	}
	// Belt-and-suspenders raw-byte checks (the user's literal failing shapes).
	if strings.Contains(string(captured), `"text":""`) {
		t.Fatalf("wire body contains an empty text block: %s", captured)
	}
	if strings.Contains(string(captured), `"content":[]`) {
		t.Fatalf("wire body contains an empty content array: %s", captured)
	}

	// And the reply is the (non-empty) second answer.
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	choices, _ := out["choices"].([]any)
	content, _ := choices[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != "second answer" {
		t.Fatalf("Turn-2 reply=%q, want 'second answer'", content)
	}
	t.Logf("Turn-2: empty assistant dropped, wire has 2 messages, NO empty text/content; reply %q. The ContentBlock empty-block error is GONE.", content)
}

// =========================================================================
// (4) UPGRADE PATH: seed OLD schema (token_id NOT NULL), run real Migrate fixup,
// then a token_id=NULL row must insert + persist (Postgres-only DROP NOT NULL).
// =========================================================================

func TestT4_UpgradePath_TokenIDDropNotNull_NilPersists(t *testing.T) {
	gdb, _, cleanup := t4Setup(t)
	defer cleanup()

	// Simulate the OLD schema: recreate request_logs with token_id NOT NULL.
	gdb.Exec("DROP TABLE IF EXISTS request_logs CASCADE")
	if err := gdb.Exec(`CREATE TABLE request_logs (
		id BIGSERIAL PRIMARY KEY,
		created_at timestamptz, updated_at timestamptz,
		user_id BIGINT NOT NULL,
		token_id BIGINT NOT NULL,
		channel_id BIGINT NOT NULL,
		rule_id BIGINT,
		model varchar(128), upstream_model varchar(128),
		inbound_format varchar(16) NOT NULL DEFAULT 'openai',
		prompt_tokens BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		status varchar(16) NOT NULL DEFAULT 'success',
		http_status BIGINT NOT NULL DEFAULT 0,
		error_message text,
		latency_ms BIGINT NOT NULL DEFAULT 0,
		is_stream boolean NOT NULL DEFAULT false
	)`).Error; err != nil {
		t.Fatalf("seed OLD schema: %v", err)
	}

	// Confirm the pre-migration constraint really is NOT NULL.
	var preNullable string
	gdb.Raw(`SELECT is_nullable FROM information_schema.columns WHERE table_name='request_logs' AND column_name='token_id'`).Scan(&preNullable)
	if preNullable != "NO" {
		t.Fatalf("pre-migration token_id is_nullable=%q, want NO (old schema not seeded correctly)", preNullable)
	}

	// A NULL token_id insert must FAIL before migration (proves the constraint bites).
	failErr := gdb.Exec(`INSERT INTO request_logs (user_id, channel_id, inbound_format, status) VALUES (1, 1, 'openai', 'success')`).Error
	if failErr == nil {
		t.Fatalf("expected NULL token_id insert to FAIL under NOT NULL, but it succeeded")
	}

	// Run the REAL migration (AutoMigrate + postMigrate DROP NOT NULL fixup).
	if err := model.Migrate(gdb); err != nil {
		t.Fatalf("Migrate (upgrade): %v", err)
	}

	// Post-migration: column must now be nullable.
	var postNullable string
	gdb.Raw(`SELECT is_nullable FROM information_schema.columns WHERE table_name='request_logs' AND column_name='token_id'`).Scan(&postNullable)
	if postNullable != "YES" {
		t.Fatalf("post-migration token_id is_nullable=%q, want YES (DROP NOT NULL fixup did not run)", postNullable)
	}

	// Now an is_test row with token_id=NULL must INSERT and PERSIST.
	logSvc := service.NewLogService(gdb)
	row := &model.RequestLog{UserID: 99, TokenID: nil, ChannelID: 1, Model: "m", InboundFormat: model.InboundOpenAI, Status: model.LogSuccess, IsTest: true}
	if err := logSvc.Write(row); err != nil {
		t.Fatalf("write nil-token_id row after upgrade: %v", err)
	}
	var got model.RequestLog
	if err := gdb.Where("is_test = ?", true).First(&got).Error; err != nil {
		t.Fatalf("load persisted nil-token_id row: %v", err)
	}
	if got.TokenID != nil {
		t.Fatalf("persisted TokenID=%v, want nil", *got.TokenID)
	}
	if got.UserID != 99 || !got.IsTest {
		t.Fatalf("persisted row mismatch: uid=%d is_test=%v", got.UserID, got.IsTest)
	}

	// postMigrate must be idempotent: a second run is a clean no-op.
	if err := model.Migrate(gdb); err != nil {
		t.Fatalf("Migrate (idempotent re-run): %v", err)
	}
	t.Logf("UPGRADE PATH: token_id NOT NULL→nullable via postMigrate; nil-token_id is_test row persisted; re-run idempotent")
}
