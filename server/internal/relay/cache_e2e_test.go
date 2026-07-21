//go:build e2e

// Package relay end-to-end prompt-cache rendering test (build tag `e2e`).
//
// This proves the "render cache tokens into the client usage" feature LIVE
// through the full Gin engine (api.New) against a real ephemeral PostgreSQL +
// Redis, over the REFORMAT path (OpenAI upstream → Anthropic output, via a key
// pinned to output_format=anthropic). The mock OpenAI upstream reports a cache
// hit under prompt_tokens_details.cached_tokens; the relay must surface it on the
// client-facing Anthropic usage as cache_read_input_tokens, on BOTH:
//
//	(non-stream) response body usage.cache_read_input_tokens == the mock value,
//	(stream)     the TERMINAL message_delta usage (NOT message_start).
//
// Cache-WRITE rendering (cache_creation_input_tokens) and the Bedrock/OpenAI
// output shapes are covered white-box in cache_render_test.go (relay package) and
// the adapter unit tests — an OpenAI upstream reports no cache-write count, so the
// end-to-end reformat proof here targets the read bucket. It reuses the e2eEnv
// harness + conventions from stream_e2e_test.go / channel_meta_e2e_test.go.
//
// Run with (DSN/addr default to the ephemeral docker containers on the host):
//
//	go test -tags e2e -run TestCacheRender ./internal/relay -v
package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/api"
	"github.com/agent-router/server/internal/db"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// wantCachedRead is the cache-hit token count the mock upstream reports (OpenAI
// prompt_tokens_details.cached_tokens) and the relay must render as the Anthropic
// cache_read_input_tokens.
const wantCachedRead = 30

// mockOpenAICacheDualMode answers BOTH non-streaming and streaming
// chat.completions, reporting a cache hit (prompt_tokens_details.cached_tokens)
// in the usage of each. It is the cache-bearing counterpart to
// mockOpenAIDualMode.
func mockOpenAICacheDualMode() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)

		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"id":"chatcmpl-cache","object":"chat.completion","model":"gpt-4o",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,`+
				`"prompt_tokens_details":{"cached_tokens":%d}}}`,
				wantPrompt, wantCompletion, wantTotal, wantCachedRead)
			return
		}

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
		// FINAL usage-bearing chunk carrying the cache hit.
		write(fmt.Sprintf("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[],"+
			"\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d,"+
			"\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\n",
			wantPrompt, wantCompletion, wantTotal, wantCachedRead))
		write("data: [DONE]\n\n")
	}))
}

// setupCacheE2E builds the same DB/Redis/engine harness as setupE2E but with the
// cache-bearing dual-mode mock, so cache-rendering assertions have a known
// cached_tokens value to compare against.
func setupCacheE2E(t *testing.T) (*e2eEnv, func()) {
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

	user := &model.User{Username: "e2e-cache", Password: "x", Role: model.RoleUser, Status: model.UserEnabled, Quota: model.QuotaUnlimited}
	if err := gdb.Create(user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	plainKey := "sk-e2e-cacherender-key"
	tok := &model.Token{
		UserID:  user.ID,
		Name:    "e2e-cache",
		KeyHash: service.HashKey(plainKey),
		Status:  model.TokenEnabled,
		Quota:   model.QuotaUnlimited,
		Group:   "default",
	}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("seed token: %v", err)
	}

	mock := mockOpenAICacheDualMode()

	chSvc := service.NewChannelService(gdb, rdb, secretKey)
	name := "mock-openai-cache"
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

// TestCacheRenderNonStreamAnthropic: a NON-streaming request with a key pinned to
// anthropic output, served by the cache-reporting OpenAI upstream, must render the
// upstream cache hit as usage.cache_read_input_tokens on the Anthropic body.
func TestCacheRenderNonStreamAnthropic(t *testing.T) {
	env, cleanup := setupCacheE2E(t)
	defer cleanup()
	env.setTokenOutputFormat(t, "anthropic")

	_, body := env.doJSON(t, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi there"}]}`)

	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("response body has no usage object: %#v", body)
	}
	got, ok := usage["cache_read_input_tokens"]
	if !ok {
		t.Fatalf("anthropic usage missing cache_read_input_tokens: %#v", usage)
	}
	// JSON numbers decode as float64.
	if int(got.(float64)) != wantCachedRead {
		t.Fatalf("cache_read_input_tokens = %v, want %d (mock upstream cached_tokens)", got, wantCachedRead)
	}
}

// TestCacheRenderStreamAnthropicTerminalDelta: a STREAMING request with a key
// pinned to anthropic output must carry the cache hit on the TERMINAL
// message_delta usage — NOT on message_start (whose usage stays input/output=0,
// per the reformat path's end-of-stream usage timing).
func TestCacheRenderStreamAnthropicTerminalDelta(t *testing.T) {
	env, cleanup := setupCacheE2E(t)
	defer cleanup()
	env.setTokenOutputFormat(t, "anthropic")

	out := env.doStream(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi there"}]}`)

	if !strings.Contains(out, "message_start") || !strings.Contains(out, "message_delta") {
		t.Fatalf("expected Anthropic framing (message_start + message_delta), got:\n%s", out)
	}

	// Isolate the message_start block and the terminal message_delta block from the
	// SSE stream (records are separated by blank lines; each has event: + data:).
	var startData, deltaData string
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		switch name {
		case "message_start":
			startData = data
		case "message_delta":
			deltaData = data // exactly one terminal message_delta in the reformat path
		}
	}
	if startData == "" || deltaData == "" {
		t.Fatalf("missing message_start/message_delta records:\n%s", out)
	}

	// message_start MUST NOT carry the cache field (end-of-stream usage timing).
	if strings.Contains(startData, "cache_read_input_tokens") {
		t.Errorf("message_start must NOT carry cache_read_input_tokens, got: %s", startData)
	}

	// Terminal message_delta MUST carry the cache read on its usage.
	var md struct {
		Usage struct {
			InputTokens     int `json:"input_tokens"`
			OutputTokens    int `json:"output_tokens"`
			CacheReadTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(deltaData), &md); err != nil {
		t.Fatalf("decode terminal message_delta: %v\ndata: %s", err, deltaData)
	}
	if md.Usage.CacheReadTokens != wantCachedRead {
		t.Fatalf("terminal message_delta cache_read_input_tokens = %d, want %d", md.Usage.CacheReadTokens, wantCachedRead)
	}
	if md.Usage.InputTokens != wantPrompt {
		t.Errorf("terminal message_delta input_tokens = %d, want %d", md.Usage.InputTokens, wantPrompt)
	}
	if md.Usage.OutputTokens != wantCompletion {
		t.Errorf("terminal message_delta output_tokens = %d, want %d", md.Usage.OutputTokens, wantCompletion)
	}
}
