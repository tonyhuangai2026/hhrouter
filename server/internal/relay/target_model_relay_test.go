package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router"
	"github.com/agent-router/server/internal/service"
)

// White-box relay tests for the rule-level model override (target_model).
//
// The engine decides the EFFECTIVE model (rule.TargetModel when set, else the
// client's requested model); relay.go / stream.go resolve the upstream model id
// from it. These tests drive the REAL serve paths against a sqlite DB and a mock
// upstream to prove the override reaches every downstream consumer of that single
// upstreamModel variable:
//
//	price lookup (billing)            — TestTargetModel_NonStream_*
//	request_logs.upstream_model        — TestTargetModel_NonStream_*, _Stream_*
//	maybeInjectSystemCache threshold   — TestTargetModel_CacheThresholdFollowsOverride
//	missing-price 400 (no failover)    — TestTargetModel_MissingPriceOnOverride*
//
// plus the failNoChannel diagnostics (response body + error_message) for an
// override nothing can serve, and the byte-for-byte legacy message when no rule
// pinned a target_model.

const tmSecret = "0123456789abcdef0123456789abcdef"

var tmDBSeq int64

// newTargetModelDB opens an isolated in-memory sqlite DB with every table the
// relay pipeline touches (routing + channels + pricing + logs + quota identity).
func newTargetModelDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:tmtest_%d?mode=memory&cache=shared", atomic.AddInt64(&tmDBSeq, 1))
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{},
		&model.RoutingRule{}, &model.RequestLog{}, &model.ModelPrice{}, &model.Option{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// tmChannel inserts an enabled channel of the given type. models/mapping are
// encoded to the JSONB columns; the upstream key is encrypted with tmSecret so
// the real ChannelService decryptor (used as the adapter Decryptor) works.
func tmChannel(t *testing.T, gdb *gorm.DB, typ model.ChannelType, baseURL string, models []string, mapping map[string]string, autoCache bool) *model.Channel {
	t.Helper()
	enc, err := crypto.Encrypt(tmSecret, "sk-upstream")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mj, _ := json.Marshal(models)
	ch := &model.Channel{
		Name:            "tm-chan",
		Type:            typ,
		BaseURL:         baseURL,
		Key:             enc,
		Models:          mj,
		Status:          model.ChannelEnabled,
		AutoCacheSystem: autoCache,
	}
	if mapping != nil {
		mm, _ := json.Marshal(mapping)
		ch.ModelMapping = mm
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch
}

// tmRule inserts an enabled routing rule carrying the given target_model (empty
// = no override). It works around the model's `default:true` Enabled tag the same
// way the engine tests do.
func tmRule(t *testing.T, gdb *gorm.DB, name, targetModel string) *model.RoutingRule {
	t.Helper()
	rule := &model.RoutingRule{
		Name:             name,
		Enabled:          true,
		Priority:         0,
		Match:            datatypes.JSON([]byte("{}")),
		TargetChannelIDs: datatypes.JSON([]byte("[]")),
		TargetModel:      targetModel,
	}
	if err := gdb.Create(rule).Error; err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return rule
}

// tmPrice inserts a (channel, model) price row.
func tmPrice(t *testing.T, gdb *gorm.DB, channelID uint, modelName string, in, out int64) {
	t.Helper()
	if err := gdb.Create(&model.ModelPrice{
		ChannelID: channelID, Model: modelName,
		InputMicroUSDPerM: in, OutputMicroUSDPerM: out,
	}).Error; err != nil {
		t.Fatalf("create price: %v", err)
	}
}

// tmIdentity seeds an unlimited-quota user + token so the USD pre-flight admits
// every request and the focus stays on model resolution.
func tmIdentity(t *testing.T, gdb *gorm.DB) (*model.User, *model.Token) {
	t.Helper()
	u := &model.User{Username: "tm-user", Password: "x", Role: model.RoleUser, Status: model.UserEnabled, Quota: model.QuotaUnlimited}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok := &model.Token{UserID: u.ID, Name: "tm-token", KeyHash: service.HashKey("sk-tm"), Status: model.TokenEnabled, Quota: model.QuotaUnlimited, Group: "default"}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}
	return u, tok
}

// tmRelayer wires a Relayer over the test DB with the real engine/services (no
// Redis: the quota service then reads/writes the DB used_quota columns).
func tmRelayer(gdb *gorm.DB) *Relayer {
	chSvc := service.NewChannelService(gdb, nil, tmSecret)
	return NewRelayer(
		router.NewEngine(gdb),
		chSvc,
		service.NewQuotaService(gdb, nil),
		service.NewLogService(gdb),
		service.NewPricingService(gdb),
		gdb,
	)
}

// tmRequestContext builds the parsed-request context the serve* paths expect.
func tmRequestContext(format InboundFormat, uni adapter.UnifiedRequest, u *model.User, tok *model.Token) *requestContext {
	return &requestContext{
		format:    format,
		outFormat: outFormatFor(format, nil),
		uni:       uni,
		token:     tok,
		user:      u,
	}
}

// tmServeNonStream drives the production serveNonStream against a recorder and
// returns the captured response.
func tmServeNonStream(r *Relayer, rc *requestContext) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	r.serveNonStream(c, rc, adapter.EstimatePromptTokens(rc.uni))
	return w
}

// tmServeStream drives the production serveStream. Streaming needs a writer that
// implements http.CloseNotifier (gin's c.Stream requirement), which the recorder
// lacks, so the handler runs behind a real TCP listener — the same pattern the
// other streaming tests use.
func tmServeStream(t *testing.T, r *Relayer, rc *requestContext) {
	t.Helper()
	front := gin.New()
	front.POST("/serve", func(c *gin.Context) {
		r.serveStream(c, rc, adapter.EstimatePromptTokens(rc.uni))
	})
	srv := httptest.NewServer(front)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/serve", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post front: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}

// tmOneLog returns the single request_logs row written by the relay.
func tmOneLog(t *testing.T, gdb *gorm.DB) model.RequestLog {
	t.Helper()
	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want exactly 1", len(rows))
	}
	return rows[0]
}

// tmErrMessage extracts the message out of an OpenAI-schema error body.
func tmErrMessage(t *testing.T, raw string) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode error body %q: %v", raw, err)
	}
	return body.Error.Message
}

// The three price tiers used by the billing tests. They are deliberately all
// different so the asserted cost identifies EXACTLY which (channel, model) row
// was consulted:
//
//	requested model "gpt-4o"                   -> 450 µUSD for the fixture usage
//	override, pre-mapping "claude-haiku-4-5"   -> 810 µUSD
//	override, post-mapping "anthropic.claude-haiku-4-5" (correct) -> 150 µUSD
const (
	tmUsagePrompt     = 100
	tmUsageCompletion = 10

	tmCostRequested     = 450 // 100*3 + 10*15
	tmCostUnmapped      = 810 // 100*7 + 10*11
	tmCostMappedTarget  = 150 // 100*1 + 10*5
	tmRequestedModel    = "gpt-4o"
	tmTargetModel       = "claude-haiku-4-5"
	tmTargetUpstreamID  = "anthropic.claude-haiku-4-5"
	tmMockCompletionTxt = "hello"
)

// tmSeedOverrideBilling seeds the shared "override to a mapped model" fixture:
// one OpenAI channel that serves the OVERRIDE model (mapped to a different
// upstream id), all three price rows, an identity and the overriding rule.
func tmSeedOverrideBilling(t *testing.T, gdb *gorm.DB, baseURL string) (*model.Channel, *model.User, *model.Token) {
	t.Helper()
	ch := tmChannel(t, gdb, model.ChannelOpenAI, baseURL,
		[]string{tmRequestedModel, tmTargetModel},
		map[string]string{tmTargetModel: tmTargetUpstreamID}, false)
	tmPrice(t, gdb, ch.ID, tmRequestedModel, 3_000_000, 15_000_000)
	tmPrice(t, gdb, ch.ID, tmTargetModel, 7_000_000, 11_000_000)
	tmPrice(t, gdb, ch.ID, tmTargetUpstreamID, 1_000_000, 5_000_000)
	tmRule(t, gdb, "haiku-tier", tmTargetModel)
	u, tok := tmIdentity(t, gdb)
	return ch, u, tok
}

// TestTargetModel_NonStream_BillingLogAndUpstreamModel is the core non-streaming
// proof: with a rule overriding the model, the upstream call, the price used for
// billing and request_logs.upstream_model all follow the OVERRIDE (through the
// channel's model_mapping), while request_logs.model keeps the client's name.
func TestTargetModel_NonStream_BillingLogAndUpstreamModel(t *testing.T) {
	gdb := newTargetModelDB(t)

	var gotUpstreamModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		gotUpstreamModel = body.Model
		fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			body.Model, tmMockCompletionTxt, tmUsagePrompt, tmUsageCompletion, tmUsagePrompt+tmUsageCompletion)
	}))
	defer srv.Close()

	_, u, tok := tmSeedOverrideBilling(t, gdb, srv.URL)
	r := tmRelayer(gdb)

	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	w := tmServeNonStream(r, rc)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The upstream saw the override AFTER model_mapping (rule layer -> channel layer).
	if gotUpstreamModel != tmTargetUpstreamID {
		t.Errorf("upstream request model = %q, want %q (override + model_mapping)", gotUpstreamModel, tmTargetUpstreamID)
	}

	log := tmOneLog(t, gdb)
	if log.Model != tmRequestedModel {
		t.Errorf("request_logs.model = %q, want the CLIENT's requested name %q", log.Model, tmRequestedModel)
	}
	if log.UpstreamModel != tmTargetUpstreamID {
		t.Errorf("request_logs.upstream_model = %q, want the overridden+mapped model %q", log.UpstreamModel, tmTargetUpstreamID)
	}
	if log.CostMicroUSD == nil {
		t.Fatal("request_logs.cost_micro_usd must be recorded on the success path")
	}
	if got := *log.CostMicroUSD; got != tmCostMappedTarget {
		t.Errorf("cost = %d µUSD, want %d (price of the OVERRIDDEN model); %d would mean the requested model's price, %d the unmapped override",
			got, tmCostMappedTarget, tmCostRequested, tmCostUnmapped)
	}
}

// TestTargetModel_Stream_BillingLogAndUpstreamModel is the same proof on the
// STREAMING path — stream.go must resolve the upstream model from the effective
// model too, not just relay.go.
func TestTargetModel_Stream_BillingLogAndUpstreamModel(t *testing.T) {
	gdb := newTargetModelDB(t)

	var gotUpstreamModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		gotUpstreamModel = body.Model

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write(fmt.Sprintf("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\n", body.Model, tmMockCompletionTxt))
		write("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write(fmt.Sprintf("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n",
			tmUsagePrompt, tmUsageCompletion, tmUsagePrompt+tmUsageCompletion))
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	_, u, tok := tmSeedOverrideBilling(t, gdb, srv.URL)
	r := tmRelayer(gdb)

	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Stream:   true,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	tmServeStream(t, r, rc)

	if gotUpstreamModel != tmTargetUpstreamID {
		t.Errorf("streaming upstream request model = %q, want %q (override + model_mapping)", gotUpstreamModel, tmTargetUpstreamID)
	}

	log := tmOneLog(t, gdb)
	if !log.IsStream {
		t.Errorf("request_logs.is_stream = false, want true")
	}
	if log.Status != model.LogSuccess {
		t.Fatalf("request_logs.status = %q (error_message=%q), want success", log.Status, log.ErrorMessage)
	}
	if log.Model != tmRequestedModel {
		t.Errorf("request_logs.model = %q, want %q", log.Model, tmRequestedModel)
	}
	if log.UpstreamModel != tmTargetUpstreamID {
		t.Errorf("request_logs.upstream_model = %q, want %q", log.UpstreamModel, tmTargetUpstreamID)
	}
	if log.CostMicroUSD == nil || *log.CostMicroUSD != tmCostMappedTarget {
		t.Errorf("cost = %v µUSD, want %d (the OVERRIDDEN model's price)", log.CostMicroUSD, tmCostMappedTarget)
	}
}

// TestTargetModel_CacheThresholdFollowsOverride is the silent-failure guard: the
// auto system-cache min length is model-dependent (haiku 4096 tokens, others
// 1024). A ~2000-token system prompt sits BETWEEN the two bars, so the same
// request either gets a breakpoint or not depending purely on which model the
// threshold was computed from. Overriding to a haiku model must therefore
// suppress the injection — if the threshold still followed the requested
// (non-haiku) model, a breakpoint would be injected that Bedrock/Anthropic
// silently ignores, i.e. caching would never hit and nothing would report it.
func TestTargetModel_CacheThresholdFollowsOverride(t *testing.T) {
	const (
		sonnetModel = "claude-sonnet-4"
		haikuModel  = "claude-haiku-4-5"
	)
	// ~2000 tokens: >= the 1024 non-haiku bar, < the 4096 haiku bar.
	midSystem := mkSystem(8000)
	if got := adapter.EstimateSystemTokens(midSystem); got < 1024 || got >= 4096 {
		t.Fatalf("fixture: system estimates %d tokens, want it in [1024,4096)", got)
	}

	// systemIsCached reports whether the outbound Anthropic body carried a system
	// cache breakpoint: buildAnthropicSystem emits a plain STRING with no
	// breakpoint and a one-element BLOCK ARRAY with one.
	systemIsCached := func(t *testing.T, targetModel string) bool {
		t.Helper()
		gdb := newTargetModelDB(t)

		var gotSystem any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			raw, _ := io.ReadAll(req.Body)
			var body struct {
				Model  string `json:"model"`
				System any    `json:"system"`
			}
			_ = json.Unmarshal(raw, &body)
			gotSystem = body.System
			fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn",`+
				`"usage":{"input_tokens":%d,"output_tokens":%d}}`, body.Model, tmMockCompletionTxt, tmUsagePrompt, tmUsageCompletion)
		}))
		defer srv.Close()

		// One anthropic channel with auto-cache ON that serves BOTH models, so the
		// only difference between the two runs is the rule's target_model.
		ch := tmChannel(t, gdb, model.ChannelAnthropic, srv.URL, []string{sonnetModel, haikuModel}, nil, true)
		tmPrice(t, gdb, ch.ID, sonnetModel, 3_000_000, 15_000_000)
		tmPrice(t, gdb, ch.ID, haikuModel, 1_000_000, 5_000_000)
		tmRule(t, gdb, "tier", targetModel)
		u, tok := tmIdentity(t, gdb)

		r := tmRelayer(gdb)
		rc := tmRequestContext(FormatAnthropic, adapter.UnifiedRequest{
			Model:    sonnetModel,
			System:   midSystem,
			Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
		}, u, tok)
		w := tmServeNonStream(r, rc)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		_, isBlockArray := gotSystem.([]any)
		return isBlockArray
	}

	// Control: no override -> threshold from the requested sonnet model (1024) ->
	// the same system IS cached. This is what makes the override case meaningful.
	if !systemIsCached(t, "") {
		t.Fatal("no override: a ~2000-token system on a non-haiku model must get a cache breakpoint (1024 bar)")
	}

	// Override to haiku -> threshold must be 4096 -> NOT cached.
	if systemIsCached(t, haikuModel) {
		t.Error("target_model overrides to a haiku model: the 4096-token bar must apply, so a ~2000-token system must NOT get a breakpoint (a 1024-bar breakpoint here would be silently ignored upstream -> caching never hits)")
	}
}

// TestTargetModel_MissingPriceOnOverrideRejects400 covers the missing-price
// semantics under an override: the price is looked up for (channel, OVERRIDDEN
// model), and when that row is absent the request is rejected 400 WITHOUT trying
// another candidate — identical to the existing missing-price behaviour, just
// keyed on the effective model. Here the REQUESTED model has a price and the
// override does not, so a 200 would prove the wrong key was used.
func TestTargetModel_MissingPriceOnOverrideRejects400(t *testing.T) {
	gdb := newTargetModelDB(t)

	var upstreamCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		fmt.Fprint(w, `{"model":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	// TWO candidate channels both serving the override, so "no failover" is
	// observable: a second attempt would call the mock upstream.
	ch1 := tmChannel(t, gdb, model.ChannelOpenAI, srv.URL, []string{tmRequestedModel, tmTargetModel}, nil, false)
	ch2 := tmChannel(t, gdb, model.ChannelOpenAI, srv.URL, []string{tmRequestedModel, tmTargetModel}, nil, false)
	// Only the REQUESTED model is priced, on both channels.
	tmPrice(t, gdb, ch1.ID, tmRequestedModel, 3_000_000, 15_000_000)
	tmPrice(t, gdb, ch2.ID, tmRequestedModel, 3_000_000, 15_000_000)
	tmRule(t, gdb, "haiku-tier", tmTargetModel)
	u, tok := tmIdentity(t, gdb)

	r := tmRelayer(gdb)
	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	w := tmServeNonStream(r, rc)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing price for the overridden model); body=%s", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (a missing price must not fail over or reach upstream)", n)
	}
	if msg := tmErrMessage(t, w.Body.String()); !strings.Contains(msg, tmTargetModel) {
		t.Errorf("error message = %q, want it to name the overridden model %q", msg, tmTargetModel)
	}

	log := tmOneLog(t, gdb)
	if log.HTTPStatus != http.StatusBadRequest || log.Status != model.LogError {
		t.Errorf("log status = (%q,%d), want (error,400)", log.Status, log.HTTPStatus)
	}
	if log.UpstreamModel != tmTargetModel {
		t.Errorf("request_logs.upstream_model = %q, want the overridden model %q", log.UpstreamModel, tmTargetModel)
	}
}

// TestTargetModel_NoCandidateSurfacesRuleAndTargetModel is the BLOCKER fix: when
// the matched rule's target_model has no candidate channel, the operator must be
// able to see WHICH rule and WHICH model were misconfigured. rc.uni.Model is the
// requested name and says nothing about the override, so the engine's wrapped
// diagnostic has to reach BOTH the response body and request_logs.error_message.
func TestTargetModel_NoCandidateSurfacesRuleAndTargetModel(t *testing.T) {
	gdb := newTargetModelDB(t)

	// The channel serves the REQUESTED model only — the override is unserved.
	ch := tmChannel(t, gdb, model.ChannelOpenAI, "http://127.0.0.1:1", []string{tmRequestedModel}, nil, false)
	tmPrice(t, gdb, ch.ID, tmRequestedModel, 3_000_000, 15_000_000)
	rule := tmRule(t, gdb, "opus-tier", tmTargetModel)
	u, tok := tmIdentity(t, gdb)

	r := tmRelayer(gdb)
	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	w := tmServeNonStream(r, rc)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (unchanged); body=%s", w.Code, w.Body.String())
	}

	// (a) the error RESPONSE BODY returned to the caller.
	respMsg := tmErrMessage(t, w.Body.String())
	// (b) request_logs.error_message, for after-the-fact triage.
	log := tmOneLog(t, gdb)

	for _, tc := range []struct{ what, got string }{
		{"error response body", respMsg},
		{"request_logs.error_message", log.ErrorMessage},
	} {
		if !strings.Contains(tc.got, rule.Name) {
			t.Errorf("%s = %q, want it to name the matched rule %q", tc.what, tc.got, rule.Name)
		}
		if !strings.Contains(tc.got, fmt.Sprintf("id %d", rule.ID)) {
			t.Errorf("%s = %q, want it to carry the rule id %d", tc.what, tc.got, rule.ID)
		}
		if !strings.Contains(tc.got, tmTargetModel) {
			t.Errorf("%s = %q, want it to carry the target_model %q", tc.what, tc.got, tmTargetModel)
		}
	}
	if log.HTTPStatus != http.StatusBadGateway || log.Status != model.LogError {
		t.Errorf("log status = (%q,%d), want (error,502)", log.Status, log.HTTPStatus)
	}
}

// TestTargetModel_NoCandidateStreamSurfacesRuleAndTargetModel: the streaming path
// funnels through the same failNoChannel, so the diagnostic must appear there too
// (this is the path Claude Code actually uses).
func TestTargetModel_NoCandidateStreamSurfacesRuleAndTargetModel(t *testing.T) {
	gdb := newTargetModelDB(t)

	ch := tmChannel(t, gdb, model.ChannelOpenAI, "http://127.0.0.1:1", []string{tmRequestedModel}, nil, false)
	tmPrice(t, gdb, ch.ID, tmRequestedModel, 3_000_000, 15_000_000)
	rule := tmRule(t, gdb, "opus-tier", tmTargetModel)
	u, tok := tmIdentity(t, gdb)

	r := tmRelayer(gdb)
	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Stream:   true,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	tmServeStream(t, r, rc)

	log := tmOneLog(t, gdb)
	if !strings.Contains(log.ErrorMessage, rule.Name) || !strings.Contains(log.ErrorMessage, tmTargetModel) {
		t.Errorf("streaming request_logs.error_message = %q, want it to carry the rule name %q and target_model %q",
			log.ErrorMessage, rule.Name, tmTargetModel)
	}
}

// TestNoOverride_NoCandidateMessageUnchanged pins the LEGACY wording for the
// no-override case (no rule pinned a target_model): operators triage on this
// exact string today, so the BLOCKER fix must not alter it — the rule/target
// detail is only appended when the engine actually has an override to report.
func TestNoOverride_NoCandidateMessageUnchanged(t *testing.T) {
	const wantMsg = `no upstream channel can serve model "gpt-4o"`

	for _, tc := range []struct {
		name   string
		mkRule bool
		stream bool
	}{
		{name: "no_rule_at_all"},
		{name: "matching_rule_without_target_model", mkRule: true},
		{name: "stream_no_rule", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gdb := newTargetModelDB(t)
			// A channel that serves nothing the request asks for.
			tmChannel(t, gdb, model.ChannelOpenAI, "http://127.0.0.1:1", []string{"some-other-model"}, nil, false)
			if tc.mkRule {
				tmRule(t, gdb, "plain-rule", "")
			}
			u, tok := tmIdentity(t, gdb)

			r := tmRelayer(gdb)
			rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
				Model:    tmRequestedModel,
				Stream:   tc.stream,
				Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
			}, u, tok)

			if tc.stream {
				tmServeStream(t, r, rc)
			} else {
				w := tmServeNonStream(r, rc)
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
				}
				if got := tmErrMessage(t, w.Body.String()); got != wantMsg {
					t.Errorf("error response body message = %q, want the unchanged legacy text %q", got, wantMsg)
				}
			}

			log := tmOneLog(t, gdb)
			if log.ErrorMessage != wantMsg {
				t.Errorf("request_logs.error_message = %q, want the unchanged legacy text %q", log.ErrorMessage, wantMsg)
			}
			if log.HTTPStatus != http.StatusBadGateway {
				t.Errorf("log HTTPStatus = %d, want 502", log.HTTPStatus)
			}
		})
	}
}

// TestNoOverride_NonErrNoCandidateStillSurfacesVerbatim guards the third branch:
// an error that is NOT ErrNoCandidate keeps its "routing failed: <err>" rendering.
func TestNoOverride_NonErrNoCandidateStillSurfacesVerbatim(t *testing.T) {
	gdb := newTargetModelDB(t)
	u, tok := tmIdentity(t, gdb)
	// Drop routing_rules so enabledRulesByPriority fails with a DB error (not
	// ErrNoCandidate), exercising the verbatim branch.
	if err := gdb.Migrator().DropTable(&model.RoutingRule{}); err != nil {
		t.Fatalf("drop routing_rules: %v", err)
	}

	r := tmRelayer(gdb)
	rc := tmRequestContext(FormatOpenAI, adapter.UnifiedRequest{
		Model:    tmRequestedModel,
		Messages: []adapter.Message{{Role: "user", Content: []adapter.ContentBlock{adapter.TextBlock("hi")}}},
	}, u, tok)
	w := tmServeNonStream(r, rc)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := tmErrMessage(t, w.Body.String()); !strings.HasPrefix(got, "routing failed: ") {
		t.Errorf("error message = %q, want the verbatim %q rendering for a non-ErrNoCandidate failure", got, "routing failed: …")
	}
}

// TestNoCandidateDetail exercises the sentinel-stripping helper directly: the
// bare sentinel yields no detail (legacy message preserved), a wrapped error
// yields only the added diagnostic, and an unrelated error is returned as-is.
func TestNoCandidateDetail(t *testing.T) {
	if got := noCandidateDetail(router.ErrNoCandidate); got != "" {
		t.Errorf("bare sentinel detail = %q, want empty (keeps the legacy message intact)", got)
	}
	wrapped := fmt.Errorf("%w: rule \"opus-tier\" (id 3) target_model \"claude-opus-4-8\" is not served by any candidate channel", router.ErrNoCandidate)
	got := noCandidateDetail(wrapped)
	if strings.Contains(got, router.ErrNoCandidate.Error()) {
		t.Errorf("detail = %q, must not repeat the sentinel text", got)
	}
	if !strings.HasPrefix(got, `rule "opus-tier" (id 3)`) || !strings.Contains(got, "claude-opus-4-8") {
		t.Errorf("detail = %q, want the rule identifier + target_model diagnostic", got)
	}
}
