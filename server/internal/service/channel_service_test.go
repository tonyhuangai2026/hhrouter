package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/model"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// newChannelTestDB opens an isolated in-memory sqlite DB with the Channel table
// migrated. datatypes.JSON maps to TEXT on sqlite, which is sufficient here.
func newChannelTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:chtest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// seedChannel inserts a channel with the given plaintext key (encrypted at rest)
// and returns its id.
func seedChannel(t *testing.T, gdb *gorm.DB, ch *model.Channel, plainKey string) uint {
	t.Helper()
	enc, err := crypto.Encrypt(testSecret, plainKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ch.Key = enc
	if ch.Models == nil {
		ch.Models = toJSONArray(nil)
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch.ID
}

// channelTestRedis returns a real Redis client when TEST_REDIS_ADDR is set,
// otherwise nil (cache-skipping paths still exercised). Mirrors the pattern in
// quota_service_test.go so the default `go test` run stays infra-free.
func channelTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		return nil
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Logf("redis %s unreachable: %v; running without cache", addr, err)
		return nil
	}
	rdb.FlushDB(ctx)
	return rdb
}

func storedModels(t *testing.T, gdb *gorm.DB, id uint) []string {
	t.Helper()
	var ch model.Channel
	if err := gdb.First(&ch, id).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	var out []string
	if len(ch.Models) > 0 {
		_ = json.Unmarshal(ch.Models, &out)
	}
	return out
}

// --- OpenAI happy path: /v1/models ------------------------------------------

func TestFetchModels_OpenAI_V1Models(t *testing.T) {
	gdb := newChannelTestDB(t)
	rdb := channelTestRedis(t)

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":""}]}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: srv.URL, Status: model.ChannelEnabled}, "sk-test")
	svc := NewChannelService(gdb, rdb, testSecret)

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if res.Source != "upstream" || res.Cached {
		t.Fatalf("source=%q cached=%v, want upstream/false", res.Source, res.Cached)
	}
	if len(res.Models) != 2 || res.Models[0] != "gpt-4o" || res.Models[1] != "gpt-4o-mini" {
		t.Fatalf("models=%v, want [gpt-4o gpt-4o-mini]", res.Models)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path=%q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth=%q, want Bearer sk-test", gotAuth)
	}
	if got := storedModels(t, gdb, id); len(got) != 2 {
		t.Fatalf("persisted models=%v, want 2", got)
	}
}

// --- OpenAI fallback: /v1/models 404 -> /models -----------------------------

func TestFetchModels_OpenAI_FallbackToModels(t *testing.T) {
	gdb := newChannelTestDB(t)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: srv.URL, Status: model.ChannelEnabled}, "sk-x")
	svc := NewChannelService(gdb, nil, testSecret)

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if res.Source != "upstream" || len(res.Models) != 1 || res.Models[0] != "local-model" {
		t.Fatalf("res=%+v, want upstream/[local-model]", res)
	}
	if len(paths) != 2 || paths[0] != "/v1/models" || paths[1] != "/models" {
		t.Fatalf("paths=%v, want [/v1/models /models]", paths)
	}
}

// base_url already ending in /v1 must not yield /v1/models then /v1/models again.
func TestFetchModels_OpenAI_FallbackDedupesV1(t *testing.T) {
	gdb := newChannelTestDB(t)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: srv.URL + "/v1", Status: model.ChannelEnabled}, "k")
	svc := NewChannelService(gdb, nil, testSecret)

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if res.Source != "upstream" || len(res.Models) != 1 {
		t.Fatalf("res=%+v, want upstream/1 model", res)
	}
	// base_url ".../v1" is normalized to the root, so the first attempt is a
	// single /v1/models (not /v1/v1/models) and the retry is /models.
	if len(paths) != 2 || paths[0] != "/v1/models" || paths[1] != "/models" {
		t.Fatalf("paths=%v, want [/v1/models /models]", paths)
	}
}

// --- OpenAI both fail: error surfaced, existing models preserved ------------

func TestFetchModels_OpenAI_BothFail_PreservesExisting(t *testing.T) {
	gdb := newChannelTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	ch := &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: srv.URL, Status: model.ChannelEnabled, Models: toJSONArray([]string{"keep-me"})}
	id := seedChannel(t, gdb, ch, "sk-bad")
	svc := NewChannelService(gdb, nil, testSecret)

	_, err := svc.FetchModels(context.Background(), id, false)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	// 401 is not 404/400, so no /models retry; error must carry status+body.
	if got := storedModels(t, gdb, id); len(got) != 1 || got[0] != "keep-me" {
		t.Fatalf("existing models overwritten: %v", got)
	}
}

// --- Bedrock live success ---------------------------------------------------

func TestFetchModels_Bedrock_LiveSuccess(t *testing.T) {
	gdb := newChannelTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept=%q, want application/json", r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer bedrock-key" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"modelSummaries":[
			{"modelId":"anthropic.claude-x","outputModalities":["TEXT"]},
			{"modelId":"amazon.titan-image","outputModalities":["IMAGE"]},
			{"modelId":"meta.llama-no-modalities"},
			{"modelId":""}
		]}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "br", Type: model.ChannelBedrock, Region: "us-east-1", Status: model.ChannelEnabled}, "bedrock-key")
	svc := NewChannelService(gdb, nil, testSecret)
	// Point the control-plane host at our mock by overriding the resolver via a
	// custom http client transport that rewrites the URL.
	svc.http = &http.Client{Transport: rewriteTransport{to: srv.URL}}

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if res.Source != "upstream" || res.Message != "" {
		t.Fatalf("res source=%q message=%q, want upstream/empty", res.Source, res.Message)
	}
	// claude-x kept (TEXT), titan-image dropped (IMAGE only), llama kept (no modalities), "" dropped.
	want := []string{"anthropic.claude-x", "meta.llama-no-modalities"}
	if len(res.Models) != len(want) || res.Models[0] != want[0] || res.Models[1] != want[1] {
		t.Fatalf("models=%v, want %v", res.Models, want)
	}
}

// --- Bedrock fallback on 403 ------------------------------------------------

func TestFetchModels_Bedrock_403FallsBack(t *testing.T) {
	gdb := newChannelTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"missing authentication token"}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "br", Type: model.ChannelBedrock, Region: "us-east-1", Status: model.ChannelEnabled}, "bedrock-key")
	svc := NewChannelService(gdb, nil, testSecret)
	svc.http = &http.Client{Transport: rewriteTransport{to: srv.URL}}

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if res.Source != "builtin" {
		t.Fatalf("source=%q, want builtin", res.Source)
	}
	if res.Message == "" {
		t.Fatal("expected non-empty fallback message")
	}
	if len(res.Models) != len(bedrockBuiltinModels) {
		t.Fatalf("models len=%d, want builtin %d", len(res.Models), len(bedrockBuiltinModels))
	}
	if got := storedModels(t, gdb, id); len(got) != len(bedrockBuiltinModels) {
		t.Fatalf("builtin list not persisted: %d", len(got))
	}
}

// --- Bedrock network error falls back ---------------------------------------

func TestFetchModels_Bedrock_NetworkErrorFallsBack(t *testing.T) {
	gdb := newChannelTestDB(t)
	id := seedChannel(t, gdb, &model.Channel{Name: "br", Type: model.ChannelBedrock, Region: "us-east-1", Status: model.ChannelEnabled}, "k")
	svc := NewChannelService(gdb, nil, testSecret)
	// Point at a closed/unroutable address.
	svc.http = &http.Client{Transport: rewriteTransport{to: "http://127.0.0.1:1"}}

	res, err := svc.FetchModels(context.Background(), id, false)
	if err != nil {
		t.Fatalf("FetchModels should not error on network failure: %v", err)
	}
	if res.Source != "builtin" || res.Message == "" {
		t.Fatalf("res=%+v, want builtin + message", res)
	}
}

// --- refresh skips cache; cache hit served when warm (requires Redis) -------

func TestFetchModels_Cache_And_Refresh(t *testing.T) {
	rdb := channelTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_ADDR not set; cache/refresh path requires Redis")
	}
	gdb := newChannelTestDB(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":[{"id":"m-` + itoa(hits) + `"}]}`))
	}))
	defer srv.Close()

	id := seedChannel(t, gdb, &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: srv.URL, Status: model.ChannelEnabled}, "k")
	svc := NewChannelService(gdb, rdb, testSecret)
	ctx := context.Background()

	r1, err := svc.FetchModels(ctx, id, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if r1.Cached || r1.Source != "upstream" {
		t.Fatalf("first fetch should be live upstream, got %+v", r1)
	}

	// Second call without refresh: served from cache, no new upstream hit.
	r2, err := svc.FetchModels(ctx, id, false)
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if !r2.Cached || r2.Source != "cache" {
		t.Fatalf("second fetch should be cached, got %+v", r2)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want 1 (cache hit on 2nd)", hits)
	}

	// refresh=true bypasses cache and re-hits upstream.
	r3, err := svc.FetchModels(ctx, id, true)
	if err != nil {
		t.Fatalf("refresh fetch: %v", err)
	}
	if r3.Cached || r3.Source != "upstream" {
		t.Fatalf("refresh fetch should be live, got %+v", r3)
	}
	if hits != 2 {
		t.Fatalf("upstream hits=%d, want 2 after refresh", hits)
	}
}

// rewriteTransport redirects every request to a fixed base URL (scheme+host),
// preserving the original request path. It lets tests intercept calls to the
// real Bedrock control-plane host without DNS tricks.
type rewriteTransport struct{ to string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := req.URL.Parse(rt.to)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestChannelService_DeleteCascadesPrices verifies that deleting a channel also
// removes its model_prices rows in the same transaction (no orphan price rows),
// and leaves another channel's prices intact.
func TestChannelService_DeleteCascadesPrices(t *testing.T) {
	dsn := "file:chcascade_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.ModelPrice{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewChannelService(gdb, nil, testSecret)

	ch1 := &model.Channel{Name: "c1", Type: model.ChannelOpenAI, BaseURL: "http://x", Status: model.ChannelEnabled}
	id1 := seedChannel(t, gdb, ch1, "k1")
	ch2 := &model.Channel{Name: "c2", Type: model.ChannelOpenAI, BaseURL: "http://y", Status: model.ChannelEnabled}
	id2 := seedChannel(t, gdb, ch2, "k2")

	// Two price rows on the channel to delete, one on the survivor.
	gdb.Create(&model.ModelPrice{ChannelID: id1, Model: "gpt-4o", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000})
	gdb.Create(&model.ModelPrice{ChannelID: id1, Model: "gpt-4o-mini", InputMicroUSDPerM: 150_000, OutputMicroUSDPerM: 600_000})
	gdb.Create(&model.ModelPrice{ChannelID: id2, Model: "claude", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000})

	if err := svc.Delete(id1); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	var c1Prices int64
	gdb.Model(&model.ModelPrice{}).Where("channel_id = ?", id1).Count(&c1Prices)
	if c1Prices != 0 {
		t.Errorf("deleted channel's price rows = %d, want 0 (cascade)", c1Prices)
	}
	var c2Prices int64
	gdb.Model(&model.ModelPrice{}).Where("channel_id = ?", id2).Count(&c2Prices)
	if c2Prices != 1 {
		t.Errorf("survivor channel's price rows = %d, want 1 (untouched)", c2Prices)
	}
}
