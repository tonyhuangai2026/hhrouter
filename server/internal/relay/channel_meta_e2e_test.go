//go:build e2e

// Package relay end-to-end channel-info exposure test (build tag `e2e`).
//
// This proves the Tech Design "expose the upstream channel that actually served
// the request" feature end to end: through the full Gin engine (api.New) against
// a real ephemeral PostgreSQL + Redis, pointed at a local MOCK upstream that
// answers BOTH non-streaming (JSON) and streaming (SSE) requests. It asserts:
//
//	(non-stream) response header X-Router-Channel-Name == the mock channel name
//	             AND the response body carries NO top-level "router" key,
//	(stream)     response header X-Router-Channel-Name == the mock channel name,
//
// over BOTH the OpenAI (/v1/chat/completions) and Anthropic (/v1/messages)
// inbound endpoints.
//
// It reuses the same DB/Redis harness conventions and the e2eEnv type from
// stream_e2e_test.go; the only difference is a dual-mode mock so the same channel
// can serve a non-streaming JSON completion as well as an SSE stream.
//
// Run with (DSN/addr default to the ephemeral docker containers on the host):
//
//	go test -tags e2e -run TestChannelInfo ./internal/relay -v
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

// channelInfoMockName is the channel name seeded for the channel-info e2e tests.
// It is plain ASCII so the X-Router-Channel-Name header value equals it verbatim
// (the percent-encoding path is covered by the unit tests).
const channelInfoMockName = "mock-openai-info"

// mockOpenAIDualMode answers BOTH non-streaming and streaming chat.completions:
// it inspects the request body's "stream" flag and returns either a single JSON
// completion (with usage) or the same SSE stream the streaming tests use.
func mockOpenAIDualMode() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)

		if !probe.Stream {
			// Non-streaming JSON completion carrying usage (so the USD billing gate
			// passes on the seeded gpt-4o / claude price rows).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"id":"chatcmpl-info","object":"chat.completion","model":"gpt-4o",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				wantPrompt, wantCompletion, wantTotal)
			return
		}

		// Streaming path: same SSE shape as mockOpenAIStream.
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
		write(fmt.Sprintf("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", wantPrompt, wantCompletion, wantTotal))
		write("data: [DONE]\n\n")
	}))
}

// setupChannelInfoE2E builds the same DB/Redis/engine harness as setupE2E but
// with the dual-mode mock and a distinctly-named channel, so channel-info
// assertions can compare against a known channel name.
func setupChannelInfoE2E(t *testing.T) (*e2eEnv, func()) {
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

	user := &model.User{Username: "e2e-info", Password: "x", Role: model.RoleUser, Status: model.UserEnabled, Quota: model.QuotaUnlimited}
	if err := gdb.Create(user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	plainKey := "sk-e2e-channelinfo-key"
	tok := &model.Token{
		UserID:  user.ID,
		Name:    "e2e-info",
		KeyHash: service.HashKey(plainKey),
		Status:  model.TokenEnabled,
		Quota:   model.QuotaUnlimited,
		Group:   "default",
	}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("seed token: %v", err)
	}

	mock := mockOpenAIDualMode()

	chSvc := service.NewChannelService(gdb, rdb, secretKey)
	name := channelInfoMockName
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

// doJSON fires one NON-streaming request through the live engine and returns the
// full HTTP response (headers + decoded body). The caller must close nothing;
// the body is fully read here.
func (e *e2eEnv) doJSON(t *testing.T, path, body string) (http.Header, map[string]any) {
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, string(raw))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode json body: %v\nraw: %s", err, string(raw))
	}
	return resp.Header, decoded
}

// doStreamHeaders fires a streamed request and returns the response headers
// (after draining the body so the stream completes cleanly).
func (e *e2eEnv) doStreamHeaders(t *testing.T, path, body string) http.Header {
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
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, string(raw))
	}
	// The X-Router-Channel-* headers are committed before the first byte of the
	// stream, so they are already present here; drain the body to finish cleanly.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header
}

// TestChannelInfoNonStreamOpenAI: a non-streaming OpenAI (/v1/chat/completions)
// request advertises the serving channel in the X-Router-Channel-Name header and
// leaves the response body untouched (NO top-level "router" key).
func TestChannelInfoNonStreamOpenAI(t *testing.T) {
	env, cleanup := setupChannelInfoE2E(t)
	defer cleanup()

	hdr, body := env.doJSON(t, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi there"}]}`)

	if got := hdr.Get("X-Router-Channel-Name"); got != channelInfoMockName {
		t.Fatalf("X-Router-Channel-Name header = %q, want %q", got, channelInfoMockName)
	}
	if _, present := body["router"]; present {
		t.Fatalf("response body must NOT carry a top-level router key (headers only): %#v", body)
	}
}

// TestChannelInfoNonStreamAnthropic: same assertions over the Anthropic
// (/v1/messages) inbound endpoint (cross-format: OpenAI upstream, Anthropic in).
func TestChannelInfoNonStreamAnthropic(t *testing.T) {
	env, cleanup := setupChannelInfoE2E(t)
	defer cleanup()

	hdr, body := env.doJSON(t, "/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`)

	if got := hdr.Get("X-Router-Channel-Name"); got != channelInfoMockName {
		t.Fatalf("X-Router-Channel-Name header = %q, want %q", got, channelInfoMockName)
	}
	if _, present := body["router"]; present {
		t.Fatalf("response body must NOT carry a top-level router key (headers only): %#v", body)
	}
}

// TestChannelInfoStreamOpenAI: a streamed OpenAI (/v1/chat/completions) request
// advertises the serving channel in the X-Router-Channel-Name response header.
func TestChannelInfoStreamOpenAI(t *testing.T) {
	env, cleanup := setupChannelInfoE2E(t)
	defer cleanup()

	hdr := env.doStreamHeaders(t, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi there"}]}`)

	if got := hdr.Get("X-Router-Channel-Name"); got != channelInfoMockName {
		t.Fatalf("X-Router-Channel-Name header = %q, want %q", got, channelInfoMockName)
	}
}

// TestChannelInfoStreamAnthropic: a streamed Anthropic (/v1/messages) request
// advertises the serving channel in the X-Router-Channel-Name response header.
func TestChannelInfoStreamAnthropic(t *testing.T) {
	env, cleanup := setupChannelInfoE2E(t)
	defer cleanup()

	hdr := env.doStreamHeaders(t, "/v1/messages",
		`{"model":"claude-3-5-sonnet","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`)

	if got := hdr.Get("X-Router-Channel-Name"); got != channelInfoMockName {
		t.Fatalf("X-Router-Channel-Name header = %q, want %q", got, channelInfoMockName)
	}
}
