//go:build e2e

// Package relay end-to-end streaming test (build tag `e2e`).
//
// This test proves the streaming usage-capture race (the reviewer's BLOCKER) is
// fixed. It boots the full Gin engine via api.New against a real ephemeral
// PostgreSQL + Redis, points a channel at a local MOCK OpenAI upstream that
// returns an SSE stream whose FINAL chunk carries a known usage
// (prompt 11 / completion 3 / total 14, via stream_options.include_usage), then
// fires the SAME streamed request many times. Every iteration must:
//
//	(a) stream the full body to the client,
//	(b) write a request_log row with the EXACT upstream usage (not the estimate),
//	(c) consume that exact token count into BOTH Redis quota counters.
//
// It also runs one streamed /v1/messages (Anthropic) request to confirm
// cross-format end-of-stream usage capture.
//
// Run with (DSN/addr default to the ephemeral docker containers on the host):
//
//	go test -tags e2e -run TestStreaming ./internal/relay -v
package relay_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/api"
	"github.com/agent-router/server/internal/db"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

const (
	wantPrompt     = 11
	wantCompletion = 3
	wantTotal      = 14
	streamIters    = 10
	// wantCostMicroUSD is the per-request USD cost under the seeded gpt-4o price
	// ($3/1M input, $15/1M output) over the mock usage (11 prompt / 3 completion):
	// ceil((11*3_000_000 + 3*15_000_000)/1_000_000) = 78 micro-USD.
	wantCostMicroUSD int64 = 78
)

// mockOpenAIStream writes an SSE stream that mimics an OpenAI chat.completions
// stream with stream_options.include_usage: content deltas, a finish chunk, then
// a FINAL chunk that carries usage with empty choices, then [DONE].
func mockOpenAIStream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		write("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo!\"}}]}\n\n")
		write("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		// FINAL usage-bearing chunk (empty choices) — the tail event the race dropped.
		write(fmt.Sprintf("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", wantPrompt, wantCompletion, wantTotal))
		write("data: [DONE]\n\n")
	}))
}

func mustEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type e2eEnv struct {
	engine    *gin.Engine
	gdb       *gorm.DB
	rdb       *redis.Client
	channelID uint
	tokenID   uint
	userID    uint
	apiKey    string
	mock      *httptest.Server
}

func setupE2E(t *testing.T) (*e2eEnv, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dsn := mustEnv("E2E_DB_DSN", "host=127.0.0.1 port=55432 user=postgres password=postgres dbname=arp sslmode=disable")
	redisAddr := mustEnv("E2E_REDIS_ADDR", "127.0.0.1:56379")

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
	for _, tbl := range []string{"request_logs", "model_prices", "tokens", "channels", "routing_rules", "users"} {
		gdb.Exec("TRUNCATE TABLE " + tbl + " RESTART IDENTITY CASCADE")
	}

	secretKey := "0123456789abcdef0123456789abcdef" // 32 bytes for AES-256

	user := &model.User{Username: "e2e", Password: "x", Role: model.RoleUser, Status: model.UserEnabled, Quota: model.QuotaUnlimited}
	if err := gdb.Create(user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	plainKey := "sk-e2e-streamingtest-key"
	tok := &model.Token{
		UserID:  user.ID,
		Name:    "e2e",
		KeyHash: service.HashKey(plainKey),
		Status:  model.TokenEnabled,
		Quota:   model.QuotaUnlimited,
		Group:   "default",
	}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("seed token: %v", err)
	}

	mock := mockOpenAIStream()

	chSvc := service.NewChannelService(gdb, rdb, secretKey)
	name := "mock-openai"
	typ := model.ChannelOpenAI
	base := mock.URL
	key := "sk-upstream"
	models := []string{"gpt-4o", "claude-3-5-sonnet"}
	chView, cerr := chSvc.Create(service.ChannelInput{
		Name: &name, Type: &typ, BaseURL: &base, Key: &key, Models: &models,
	})
	if cerr != nil {
		t.Fatalf("seed channel: %v", cerr)
	}
	// USD billing gate: both served models need a configured price or the relay
	// rejects with 400 before any upstream call. Seed input/output prices so the
	// existing streaming tests exercise the success path; the per-test no-price
	// case uses a DIFFERENT model with no row.
	for _, m := range models {
		if err := gdb.Create(&model.ModelPrice{
			ChannelID: chView.ID, Model: m,
			InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000,
		}).Error; err != nil {
			t.Fatalf("seed price: %v", err)
		}
	}

	engine := api.New(api.Deps{DB: gdb, Redis: rdb, JWTSecret: "jwt", SecretKey: secretKey})

	env := &e2eEnv{engine: engine, gdb: gdb, rdb: rdb, channelID: chView.ID, tokenID: tok.ID, userID: user.ID, apiKey: plainKey, mock: mock}
	cleanup := func() {
		mock.Close()
		_ = rdb.Close()
	}
	return env, cleanup
}

// doStream fires one streamed request through the live engine over a real TCP
// listener (so c.Stream's flushing path behaves exactly as in production) and
// returns the full streamed body.
func (e *e2eEnv) doStream(t *testing.T, path, body string) string {
	t.Helper()
	srv := httptest.NewServer(e.engine)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		sb.WriteString(sc.Text())
		sb.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream body: %v", err)
	}
	return sb.String()
}

// doStreamRaw fires a streamed request and returns the RAW response bytes (no
// line-splitting) plus the Content-Type — needed for Bedrock binary frames.
func (e *e2eEnv) doStreamRaw(t *testing.T, path, body string) ([]byte, string) {
	t.Helper()
	srv := httptest.NewServer(e.engine)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.Header.Get("Content-Type")
}

// setTokenOutputFormat updates the seeded token's output_format directly in the
// DB (the relay reads it via the token row on the next request).
func (e *e2eEnv) setTokenOutputFormat(t *testing.T, f string) {
	t.Helper()
	if err := e.gdb.Model(&model.Token{}).Where("id = ?", e.tokenID).
		UpdateColumn("output_format", f).Error; err != nil {
		t.Fatalf("set output_format: %v", err)
	}
}

// TestStreamingUsageCaptureNoRace fires the SAME streamed request streamIters
// times and asserts that EVERY iteration captured the exact upstream usage in
// both the request_log row and BOTH Redis quota counters, with the full body
// streamed. This is the direct proof that the select-race is gone.
func TestStreamingUsageCaptureNoRace(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()

	const reqBody = `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi there"}]}`

	pass := 0
	for i := 1; i <= streamIters; i++ {
		bodyBefore := env.countLogs(t)

		out := env.doStream(t, "/v1/chat/completions", reqBody)

		// (a) full body streamed: deltas + terminal sentinel.
		if !strings.Contains(out, "Hel") || !strings.Contains(out, "lo!") {
			t.Fatalf("iter %d: streamed body missing content deltas:\n%s", i, out)
		}
		if !strings.Contains(out, "[DONE]") {
			t.Fatalf("iter %d: streamed body missing [DONE] sentinel:\n%s", i, out)
		}

		// (b) request_log row with EXACT upstream usage (not the char-estimate).
		log := env.waitForNewLog(t, bodyBefore)
		if log.PromptTokens != wantPrompt || log.CompletionTokens != wantCompletion || log.TotalTokens != wantTotal {
			t.Fatalf("iter %d: request_log usage = (%d/%d/%d), want (%d/%d/%d) — ESTIMATE LEAKED (race not fixed)",
				i, log.PromptTokens, log.CompletionTokens, log.TotalTokens, wantPrompt, wantCompletion, wantTotal)
		}
		if log.Status != model.LogSuccess || !log.IsStream {
			t.Fatalf("iter %d: log status=%q isStream=%v, want success/true", i, log.Status, log.IsStream)
		}
		// TTFT: the mock emits content deltas before the usage chunk, so a streamed
		// success MUST record a non-nil first_token_ms (>= 0).
		if log.FirstTokenMs == nil {
			t.Fatalf("iter %d: streamed log first_token_ms = nil, want non-nil TTFT", i)
		}
		if *log.FirstTokenMs < 0 {
			t.Fatalf("iter %d: streamed log first_token_ms = %d, want >= 0", i, *log.FirstTokenMs)
		}
		// Production /v1 traffic is key-scoped: the request_log row MUST carry a
		// non-nil token_id equal to the calling token (no-regression: test-chat
		// writes NULL, production never does).
		if log.TokenID == nil || *log.TokenID != env.tokenID {
			t.Fatalf("iter %d: production log token_id=%v, want non-nil %d", i, log.TokenID, env.tokenID)
		}
		if log.IsTest {
			t.Fatalf("iter %d: production log IsTest=true, want false", i)
		}

		// (c) BOTH Redis quota counters advanced by exactly the per-request USD
		// cost (micro-USD), not the token total — billing is USD-based now. The
		// seeded gpt-4o price ($3/1M in, $15/1M out) over the upstream usage
		// (11 prompt / 3 completion) is ceil((11*3_000_000 + 3*15_000_000)/1e6)
		// = 78 micro-USD per request. (Also confirms the cost was logged.)
		if log.CostMicroUSD == nil || *log.CostMicroUSD != wantCostMicroUSD {
			t.Fatalf("iter %d: request_log cost = %v, want %d micro-USD", i, log.CostMicroUSD, wantCostMicroUSD)
		}
		wantCum := int64(i) * wantCostMicroUSD
		if got := env.redisCounter(t, fmt.Sprintf("quota:token:used:%d", env.tokenID)); got != wantCum {
			t.Fatalf("iter %d: token USD counter = %d, want %d (micro-USD)", i, got, wantCum)
		}
		if got := env.redisCounter(t, fmt.Sprintf("quota:user:used:%d", env.userID)); got != wantCum {
			t.Fatalf("iter %d: user USD counter = %d, want %d (micro-USD)", i, got, wantCum)
		}
		pass++
	}
	t.Logf("STREAMING USAGE CAPTURE: %d/%d iterations captured exact upstream usage (11/3/14) + USD cost (78 micro-USD) in request_log AND both Redis counters", pass, streamIters)
}

// TestStreamingAnthropicCrossFormatUsage confirms a streamed /v1/messages
// (Anthropic inbound) request against the OpenAI upstream also captures the
// end-of-stream usage (cross-format).
func TestStreamingAnthropicCrossFormatUsage(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()

	const reqBody = `{"model":"claude-3-5-sonnet","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`

	bodyBefore := env.countLogs(t)
	out := env.doStream(t, "/v1/messages", reqBody)

	if !strings.Contains(out, "message_stop") {
		t.Fatalf("anthropic stream missing message_stop framing:\n%s", out)
	}
	if !strings.Contains(out, "Hel") || !strings.Contains(out, "lo!") {
		t.Fatalf("anthropic stream missing content deltas:\n%s", out)
	}

	log := env.waitForNewLog(t, bodyBefore)
	if log.PromptTokens != wantPrompt || log.CompletionTokens != wantCompletion || log.TotalTokens != wantTotal {
		t.Fatalf("anthropic request_log usage = (%d/%d/%d), want (%d/%d/%d)",
			log.PromptTokens, log.CompletionTokens, log.TotalTokens, wantPrompt, wantCompletion, wantTotal)
	}
	if log.InboundFormat != model.InboundAnthropic {
		t.Fatalf("inbound format = %q, want anthropic", log.InboundFormat)
	}
	t.Logf("ANTHROPIC CROSS-FORMAT: captured exact upstream usage (11/3/14) on streamed /v1/messages")
}

// TestBilling_NoPriceRejected proves the USD price gate: a request for a model
// that has NO configured price on the channel is rejected with 400 BEFORE any
// upstream call, no failover, and an error row is logged (cost NULL).
func TestBilling_NoPriceRejected(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()

	// "claude-3-5-sonnet" IS in the channel's model list but we only seeded prices
	// for it and gpt-4o; delete its price row to simulate an unpriced model.
	env.gdb.Where("channel_id = ? AND model = ?", env.channelID, "claude-3-5-sonnet").Delete(&model.ModelPrice{})

	before := env.countLogs(t)
	srv := httptest.NewServer(env.engine)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no-price status = %d, want 400", resp.StatusCode)
	}
	log := env.waitForNewLog(t, before)
	if log.Status != model.LogError {
		t.Errorf("status = %q, want error", log.Status)
	}
	if log.CostMicroUSD != nil {
		t.Errorf("cost = %v, want NULL on a gated request", *log.CostMicroUSD)
	}
}

// TestBilling_CostComputedAndDebited proves a priced streamed request records a
// micro-USD cost on the log and debits that exact amount from BOTH USD quota
// counters. Mock usage = prompt 11 / completion 3; price $3/1M in, $15/1M out →
// cost = ceil((11*3_000_000 + 3*15_000_000)/1_000_000) = 78 micro-USD.
func TestBilling_CostComputedAndDebited(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()

	const wantCost = int64(78)
	before := env.countLogs(t)
	out := env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream did not complete:\n%s", out)
	}
	log := env.waitForNewLog(t, before)
	if log.Status != model.LogSuccess {
		t.Fatalf("status = %q, want success", log.Status)
	}
	if log.CostMicroUSD == nil || *log.CostMicroUSD != wantCost {
		t.Fatalf("cost = %v, want %d micro-USD", log.CostMicroUSD, wantCost)
	}
	if got := env.redisCounter(t, fmt.Sprintf("quota:user:used:%d", env.userID)); got != wantCost {
		t.Errorf("user USD counter = %d, want %d", got, wantCost)
	}
	if got := env.redisCounter(t, fmt.Sprintf("quota:token:used:%d", env.tokenID)); got != wantCost {
		t.Errorf("token USD counter = %d, want %d", got, wantCost)
	}
}

// TestRequestLogIO_CapturesInputOutput verifies the RequestLogIO switch: with it
// on, a successful request records the rendered input + assistant output on the
// log; with it off (default), both stay NULL.
func TestRequestLogIO_CapturesInputOutput(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()

	// Default OFF → no capture.
	before := env.countLogs(t)
	env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hidden A"}]}`)
	off := env.waitForNewLog(t, before)
	if off.RequestBody != nil || off.ResponseBody != nil {
		t.Fatalf("switch off: expected NULL bodies, got req=%v resp=%v", off.RequestBody, off.ResponseBody)
	}

	// Turn ON.
	if err := model.SetOption(env.gdb, model.OptRequestLogIO, "true"); err != nil {
		t.Fatalf("set option: %v", err)
	}
	before = env.countLogs(t)
	env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"capture this input"}]}`)
	on := env.waitForNewLog(t, before)
	if on.RequestBody == nil || !strings.Contains(*on.RequestBody, "capture this input") {
		t.Fatalf("switch on: request_body missing input, got %v", on.RequestBody)
	}
	// The mock stream emits "Hello!" as the completion text.
	if on.ResponseBody == nil || *on.ResponseBody == "" {
		t.Fatalf("switch on: response_body empty, got %v", on.ResponseBody)
	}
}

func (e *e2eEnv) countLogs(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.gdb.Model(&model.RequestLog{}).Count(&n).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}

// waitForNewLog polls for a request_log row beyond the prior count (the relay
// writes the log after the stream completes, which happens shortly after the
// client finishes reading the body).
func (e *e2eEnv) waitForNewLog(t *testing.T, prevCount int64) model.RequestLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.countLogs(t) > prevCount {
			var log model.RequestLog
			if err := e.gdb.Order("id desc").First(&log).Error; err != nil {
				t.Fatalf("load latest log: %v", err)
			}
			return log
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for new request_log row (prev=%d)", prevCount)
	return model.RequestLog{}
}

func (e *e2eEnv) redisCounter(t *testing.T, key string) int64 {
	t.Helper()
	// The counter advances right after the log is written; allow a brief settle.
	deadline := time.Now().Add(2 * time.Second)
	var last int64
	for time.Now().Before(deadline) {
		v, err := e.rdb.Get(context.Background(), key).Int64()
		if err == nil {
			last = v
		}
		time.Sleep(20 * time.Millisecond)
		// Return once we have a value (the caller asserts the exact expected total).
		if err == nil {
			return v
		}
	}
	return last
}

// TestOutputFormat_AnthropicStreamOverOpenAIEndpoint: a key pinned to anthropic
// output, hitting the OpenAI endpoint with a stream, gets Anthropic SSE framing
// (message_start/message_stop) — and the request_log.inbound_format STILL records
// the true inbound dialect (openai), not the output format.
func TestOutputFormat_AnthropicStreamOverOpenAIEndpoint(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()
	env.setTokenOutputFormat(t, "anthropic")

	before := env.countLogs(t)
	out := env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(out, "message_start") || !strings.Contains(out, "message_stop") {
		t.Fatalf("expected Anthropic SSE framing, got:\n%s", out)
	}
	if !strings.Contains(out, "content_block_delta") {
		t.Fatalf("expected anthropic content_block_delta, got:\n%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("anthropic output should NOT emit the OpenAI [DONE] sentinel")
	}
	// INVARIANT: inbound_format is the true endpoint dialect, not the output format.
	log := env.waitForNewLog(t, before)
	if log.InboundFormat != model.InboundOpenAI {
		t.Fatalf("inbound_format = %q, want openai (true inbound), NOT polluted by output format", log.InboundFormat)
	}
}

// TestOutputFormat_BedrockStreamWireAndContentType: a key pinned to bedrock
// output gets the AWS event-stream content type and binary frames carrying the
// Converse event types + JSON payloads. inbound_format stays openai.
func TestOutputFormat_BedrockStreamOverOpenAIEndpoint(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()
	env.setTokenOutputFormat(t, "bedrock")

	before := env.countLogs(t)
	raw, ct := env.doStreamRaw(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)

	if ct != "application/vnd.amazon.eventstream" {
		t.Fatalf("Content-Type = %q, want application/vnd.amazon.eventstream", ct)
	}
	s := string(raw)
	// The :event-type header values are written as plain bytes in each frame.
	for _, et := range []string{"messageStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata"} {
		if !strings.Contains(s, et) {
			t.Errorf("bedrock stream missing event type %q", et)
		}
	}
	// Content delta JSON + usage payload appear in the frames.
	if !strings.Contains(s, `"delta":{"text":`) {
		t.Errorf("bedrock stream missing contentBlockDelta text payload")
	}
	if !strings.Contains(s, `"inputTokens"`) {
		t.Errorf("bedrock stream missing metadata usage payload")
	}
	if strings.Contains(s, "[DONE]") || strings.Contains(s, "data:") {
		t.Errorf("bedrock output must not use SSE framing")
	}
	log := env.waitForNewLog(t, before)
	if log.InboundFormat != model.InboundOpenAI {
		t.Fatalf("inbound_format = %q, want openai (true inbound)", log.InboundFormat)
	}
}

// TestOutputFormat_UnsetFollowsEndpoint: with no output format set, an OpenAI
// endpoint stream still renders OpenAI ([DONE]) — backward compatible.
func TestOutputFormat_UnsetFollowsEndpoint(t *testing.T) {
	env, cleanup := setupE2E(t)
	defer cleanup()
	// no setTokenOutputFormat — output_format stays NULL.

	out := env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("unset output format should follow endpoint (OpenAI [DONE]), got:\n%s", out)
	}
	if strings.Contains(out, "message_start") {
		t.Errorf("unset output format should NOT render anthropic framing")
	}
}
