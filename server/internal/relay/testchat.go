package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// ctxUserID is the gin context key under which JWTAuth stores the authenticated
// admin's user id (mirrors middleware.CtxUserID). It is referenced by its string
// value rather than importing the middleware package, because middleware imports
// relay (quota.go / relay_auth.go) and the reverse import would be a cycle —
// exactly as tokenFromCtx/userFromCtx read their keys in relay.go.
const ctxUserID = "uid"

// testchat.go implements the admin direct test-chat path (Tech Design §3): a
// thin relay against ONE explicitly chosen channel, deliberately DECOUPLED from
// the production /v1 relay pipeline. It does NOT:
//   - select a channel via the routing engine (the channel is given by :id),
//   - run or consult the quota service (the route is not mounted behind Quota
//     and this code NEVER calls Consume/CheckRemaining — test traffic must never
//     debit a budget).
//
// It DOES (Tech Design §3.2) write exactly one request_log row per completion
// (stream and non-stream, success and error) tagged is_test=true with
// token_id=nil, so test traffic is auditable in the dashboard yet excluded from
// the production summary/timeseries by default. A log-write failure only warns
// and never affects the chat response.
//
// It also reuses the pure adapter layer (adapter.For / BuildRequest /
// ParseResponse / ParseStreamChunk), the OpenAI request/response transforms, and
// the package-local SSE helpers (setSSEHeaders / writeSSERaw / upstreamEvents),
// so streaming and Bedrock event-stream de-framing behave identically to /v1.

// testChatTimeout bounds a single non-streaming upstream attempt for test-chat.
const testChatTimeout = 120 * time.Second

// TestChatController serves POST /api/channels/:id/test-chat. It holds the
// ChannelService (used both to load+decrypt the channel and as the
// adapter.Decryptor), the upstream HTTP clients, and the LogService used to
// write the is_test audit row — but NO routing engine and NO quota service.
type TestChatController struct {
	channels *service.ChannelService
	logs     *service.LogService
	pricing  *service.PricingService
	httpDo   func(*http.Request) (*http.Response, error)
	streamDo func(*http.Request) (*http.Response, error)
}

// NewTestChatController constructs a TestChatController from the channel service,
// the log service, and the pricing service. The LogService is injected (per Tech
// Design §3.2) so test-chat can persist an is_test request_log; it may be nil in
// tests that do not exercise logging. The PricingService enforces the same price
// gate as production (Tech Design §4.4): test-chat rejects an unpriced model and
// records the computed cost on the is_test log — but never consumes quota. It may
// be nil in tests that don't exercise pricing (the gate is then skipped).
func NewTestChatController(channels *service.ChannelService, logs *service.LogService, pricing *service.PricingService) *TestChatController {
	nonStream := &http.Client{Timeout: testChatTimeout}
	stream := &http.Client{}
	return &TestChatController{
		channels: channels,
		logs:     logs,
		pricing:  pricing,
		httpDo:   nonStream.Do,
		streamDo: stream.Do,
	}
}

// testChatRequest is the OpenAI chat-style body accepted by test-chat. Model is
// optional (resolved against the channel's model list when omitted). Messages
// reuse the inbound OpenAI message shape so content may be a plain string or a
// parts array (including image_url parts, base64 data: URL or http URL).
type testChatRequest struct {
	Model       string                         `json:"model"`
	Messages    []adapter.OpenAIInboundMessage `json:"messages"`
	Stream      bool                           `json:"stream"`
	MaxTokens   *int                           `json:"max_tokens"`
	Temperature *float64                       `json:"temperature"`
	TopP        *float64                       `json:"top_p"`
}

// TestChat handles POST /api/channels/:id/test-chat. It is mounted behind
// JWTAuth+AdminOnly (see api/router.go); auth/authorization are enforced by that
// middleware chain, so a missing token yields 401 and a non-admin yields 403
// before this handler runs.
func (tc *TestChatController) TestChat(c *gin.Context) {
	id, ok := parseTestChannelID(c)
	if !ok {
		return
	}

	var req testChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", "field \"messages\" is required and must be non-empty")
		return
	}

	// Load + decrypt the target channel directly (NO routing engine).
	ch, err := tc.channels.GetRaw(id)
	if err != nil {
		if errors.Is(err, service.ErrChannelNotFound) {
			writeTestChatError(c, http.StatusNotFound, "invalid_request_error", "channel not found")
			return
		}
		writeTestChatError(c, http.StatusInternalServerError, "internal_error", "could not load channel")
		return
	}

	ad, ok := adapter.For(ch, tc.channels)
	if !ok {
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", "unsupported channel type \""+string(ch.Type)+"\"")
		return
	}

	// Resolve the model: explicit > sole channel model; surface a non-blocking
	// warning when the chosen model is not in the channel's listed models.
	models := decodeChannelModels(ch.Models)
	resolvedModel, warning, rerr := resolveTestModel(req.Model, models)
	if rerr != "" {
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", rerr)
		return
	}

	uni := buildTestUnifiedRequest(req, resolvedModel)

	// From here on an upstream attempt is made, so we record exactly one is_test
	// request_log capturing its outcome. The serve methods fill in the outcome
	// fields; the log is written once, after dispatch, regardless of which exit
	// path the serve method took. (Tech Design §3.2)
	st := &testLogState{
		channelID:     ch.ID,
		model:         resolvedModel,
		upstreamModel: resolvedModel,
		isStream:      uni.Stream,
		start:         time.Now(),
		// Optimistic default; the serve methods override on any failure.
		status:     model.LogSuccess,
		httpStatus: http.StatusOK,
	}
	defer tc.writeTestLog(c, st)

	// Price gate (Tech Design §4.4): the same "model must have a price" rule as
	// production /v1. A missing price → 400, no upstream call. test-chat still
	// never consumes quota, but the resolved price is kept so writeTestLog can
	// record the computed cost on the is_test log. Skipped when no PricingService
	// is wired (bare-handler tests).
	if tc.pricing != nil {
		price, perr := tc.pricing.Lookup(ch.ID, resolvedModel)
		if perr != nil {
			if errors.Is(perr, service.ErrPriceNotConfigured) {
				msg := "model \"" + resolvedModel + "\" has no price configured on channel \"" + ch.Name + "\"; configure a price before testing"
				st.fail(http.StatusBadRequest, msg)
				writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", msg)
				return
			}
			msg := "could not look up model price: " + perr.Error()
			st.fail(http.StatusInternalServerError, msg)
			writeTestChatError(c, http.StatusInternalServerError, "internal_error", msg)
			return
		}
		st.price = price
	}

	if uni.Stream {
		tc.serveStream(c, ad, ch, uni, warning, st)
		return
	}
	tc.serveNonStream(c, ad, ch, uni, warning, st)
}

// testLogState accumulates the fields of the single is_test request_log written
// per test-chat attempt. The serve methods mutate it in place as the outcome
// becomes known; writeTestLog persists it once at the end.
type testLogState struct {
	channelID     uint
	model         string
	upstreamModel string
	isStream      bool
	start         time.Time

	status     model.LogStatus
	httpStatus int
	errMsg     string

	promptTokens     int
	completionTokens int
	totalTokens      int
	cacheReadTokens  int
	cacheWriteTokens int

	// firstTokenMs is the streamed time-to-first-token in milliseconds, set by
	// serveStream from the pump's measured TTFT. Nil for non-stream attempts and
	// for streams that errored before any content delta.
	firstTokenMs *int

	// price is the resolved (channel, model) price (nil when no PricingService is
	// wired). writeTestLog computes the request's micro-USD cost from it × the
	// captured usage and records it on the is_test log. test-chat NEVER consumes
	// quota regardless.
	price *model.ModelPrice
}

// fail records an error outcome (status=error) on the log state.
func (st *testLogState) fail(httpStatus int, errMsg string) {
	st.status = model.LogError
	st.httpStatus = httpStatus
	st.errMsg = errMsg
}

// setUsage records the resolved token counts on the log state.
func (st *testLogState) setUsage(prompt, completion, total int) {
	st.promptTokens = prompt
	st.completionTokens = completion
	st.totalTokens = total
}

// setCache records the prompt-cache token counts (for cost + the log's cache
// columns). Called only where the upstream reported real usage.
func (st *testLogState) setCache(cacheRead, cacheWrite int) {
	st.cacheReadTokens = cacheRead
	st.cacheWriteTokens = cacheWrite
}

// writeTestLog persists the single is_test request_log for a test-chat attempt.
// It is best-effort: a nil LogService (test wiring) or a write error only warns
// and never affects the already-sent chat response (Tech Design §3.2). The row
// is always is_test=true with token_id=nil (test traffic is not key-scoped) and
// records the current admin uid from the JWT context.
func (tc *TestChatController) writeTestLog(c *gin.Context, st *testLogState) {
	if tc.logs == nil {
		return
	}
	uid := userIDFromCtx(c)
	row := &model.RequestLog{
		UserID:           uid,
		TokenID:          nil, // test-chat is not associated with any token
		ChannelID:        st.channelID,
		Model:            st.model,
		UpstreamModel:    st.upstreamModel,
		InboundFormat:    model.InboundOpenAI, // test-chat always speaks OpenAI
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		TotalTokens:      st.totalTokens,
		Status:           st.status,
		HTTPStatus:       st.httpStatus,
		ErrorMessage:     st.errMsg,
		LatencyMs:        int(time.Since(st.start).Milliseconds()),
		FirstTokenMs:     st.firstTokenMs,
		IsStream:         st.isStream,
		IsTest:           true,
	}
	// Cost: when a price was resolved (gate passed) and the attempt succeeded,
	// record the cache tokens + computed micro-USD cost. test-chat NEVER consumes
	// quota — this is purely so admins can see per-request cost in Playground logs.
	if st.price != nil && tc.pricing != nil && st.status == model.LogSuccess {
		cr := st.cacheReadTokens
		cw := st.cacheWriteTokens
		row.CacheReadTokens = &cr
		row.CacheWriteTokens = &cw
		cost := tc.pricing.Cost(st.price, adapter.Usage{
			PromptTokens:     st.promptTokens,
			CompletionTokens: st.completionTokens,
			CacheReadTokens:  st.cacheReadTokens,
			CacheWriteTokens: st.cacheWriteTokens,
		})
		row.CostMicroUSD = &cost
	}
	if err := tc.logs.Write(row); err != nil {
		log.Printf("test-chat: failed to write request_log (channel=%d model=%q): %v", st.channelID, st.model, err)
	}
}

// userIDFromCtx pulls the authenticated admin's uid that JWTAuth stored under
// the "uid" context key. Returns 0 when absent (e.g. the bare-handler tests
// that mount test-chat without the middleware chain).
func userIDFromCtx(c *gin.Context) uint {
	v, ok := c.Get(ctxUserID)
	if !ok {
		return 0
	}
	id, _ := v.(uint)
	return id
}

// serveNonStream performs a single non-streaming upstream call and renders the
// OpenAI chat-completion response. No quota is consumed; the outcome (status,
// http status, error text, token usage) is recorded on st for the is_test log.
func (tc *TestChatController) serveNonStream(c *gin.Context, ad adapter.Adapter, ch *model.Channel, uni adapter.UnifiedRequest, warning string, st *testLogState) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), testChatTimeout)
	defer cancel()

	req, err := ad.BuildRequest(ctx, uni, ch)
	if err != nil {
		// A build failure here is a request-construction problem (e.g. an image
		// URL that could not be downloaded for Bedrock): treat as a bad request.
		msg := "could not build upstream request: " + err.Error()
		st.fail(http.StatusBadRequest, msg)
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}

	resp, err := tc.httpDo(req)
	if err != nil {
		msg := "upstream request failed: " + err.Error()
		st.fail(http.StatusBadGateway, msg)
		writeTestChatError(c, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	uniResp, usage, err := ad.ParseResponse(resp)
	_ = resp.Body.Close()
	if err != nil {
		tc.recordUpstreamError(st, err)
		tc.writeUpstreamError(c, err)
		return
	}

	st.setUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	st.setCache(usage.CacheReadTokens, usage.CacheWriteTokens)

	uniResp.Model = uni.Model
	body := adapter.BuildOpenAIResponse(uniResp)
	body["id"] = "chatcmpl-" + newID()
	body["created"] = time.Now().Unix()
	if warning != "" {
		body["warning"] = warning
	}
	c.JSON(http.StatusOK, body)
}

// serveStream opens the upstream stream and re-emits each chunk as an OpenAI SSE
// `data: {choices:[{delta}]}` line, terminated by `data: [DONE]`. Bedrock
// upstreams are de-framed by the shared upstreamEvents helper. No quota is
// consumed; the outcome (final usage, or a connect/mid-stream error) is recorded
// on st for the is_test log.
func (tc *TestChatController) serveStream(c *gin.Context, ad adapter.Adapter, ch *model.Channel, uni adapter.UnifiedRequest, warning string, st *testLogState) {
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	uni.Stream = true
	req, err := ad.BuildRequest(ctx, uni, ch)
	if err != nil {
		msg := "could not build upstream request: " + err.Error()
		st.fail(http.StatusBadRequest, msg)
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}

	resp, err := tc.streamDo(req)
	if err != nil {
		msg := "upstream request failed: " + err.Error()
		st.fail(http.StatusBadGateway, msg)
		writeTestChatError(c, http.StatusBadGateway, "upstream_error", msg)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := strings.TrimSpace(string(bodyBytes))
		if msg == "" {
			msg = resp.Status
		}
		ue := &adapter.UpstreamError{Provider: ad.Name(), StatusCode: resp.StatusCode, Message: msg}
		tc.recordUpstreamError(st, ue)
		tc.writeUpstreamError(c, ue)
		return
	}

	// Headers committed: from here on errors can only terminate the stream.
	setSSEHeaders(c)
	if warning != "" {
		// Emit the non-blocking warning as a leading SSE comment so it does not
		// interfere with delta parsing on the client.
		writeSSEComment(c, "warning: "+warning)
	}

	// Anchor TTFT at the request start (st.start, same origin as total latency),
	// NOT here: tc.streamDo above already returned once the upstream flushed its
	// response headers, which providers send together with — or right before —
	// the first content event, so measuring from this point yields ~0ms and
	// misses the whole "waiting for the model to start generating" interval that
	// IS the time-to-first-token. (Production /v1 anchors at serveStream start
	// for the same reason.)
	usage, completionText, firstToken, streamErr := tc.pumpOpenAIStream(c, ad, resp, uni.Model, st.start)
	writeSSERaw(c, "[DONE]")

	if firstToken != nil {
		ms := int(firstToken.Milliseconds())
		st.firstTokenMs = &ms
	}

	// Record the streamed outcome for the is_test log: prefer upstream-reported
	// usage, else fall back to a char-based completion estimate (the prompt count
	// is unknown for the stream path, so it stays 0).
	if usage != nil {
		st.setUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
		st.setCache(usage.CacheReadTokens, usage.CacheWriteTokens)
	} else if completionText != "" {
		comp := estimateTextTokens(completionText)
		st.setUsage(0, comp, comp)
	}
	if streamErr != nil {
		// SSE bytes were already flushed (200 committed to the client), but the
		// is_test log records the error for visibility. For a fatal upstream event
		// (e.g. a Bedrock *Exception) we log its mapped upstream status; otherwise
		// the status stays 200.
		httpStatus := http.StatusOK
		if mapped := upstreamHTTPStatus(streamErr); mapped != 0 {
			httpStatus = mapped
		}
		st.fail(httpStatus, streamErr.Error())
	} else if completionText == "" && usage == nil {
		// Clean end-of-stream with no content and no usage: surface a readable
		// warning so it is distinguishable from a genuine non-empty reply. The
		// HTTP response already succeeded (200), so this is logged as a warning on
		// a success row rather than an error.
		st.errMsg = emptyStreamWarning
	}
}

// pumpOpenAIStream drains the upstream events, converts each to an OpenAI stream
// chunk and writes it. It mirrors the production pump's drain-then-terminal-error
// discipline but emits ONLY the OpenAI dialect (the inbound format is fixed to
// OpenAI for test-chat). It accumulates the final upstream usage (nil if never
// reported) and the concatenated completion text (for an estimate fallback), and
// returns any fatal mid-stream error.
func (tc *TestChatController) pumpOpenAIStream(c *gin.Context, ad adapter.Adapter, resp *http.Response, modelID string, streamStart time.Time) (*adapter.Usage, string, *time.Duration, error) {
	events, errCh := upstreamEvents(ad.Name(), resp.Body)

	var (
		finalUsage   *adapter.Usage
		completion   strings.Builder
		fatalErr     error
		firstTokenAt *time.Time // wall-clock of the first non-empty delta (TTFT anchor)
	)
	clientGone := false
	process := func(ev streamEvent) {
		chunk, meaningful, perr := ad.ParseStreamChunk(ev.EventType, ev.Payload)
		if perr != nil {
			return
		}
		// A fatal upstream error (e.g. a Bedrock *Exception event) terminates the
		// stream with an error outcome — it must NOT be swallowed like a malformed
		// frame (the bug B2 fixes: both pumps did `if perr != nil { return }`).
		if chunk.UpstreamErr != nil {
			fatalErr = chunk.UpstreamErr
			return
		}
		if u := chunk.Usage; u != nil {
			// Merge (not replace): Anthropic splits prompt tokens (message_start)
			// and completion tokens (message_delta) across two events. Emit the
			// MERGED running total on the chunk so the client (which keeps the
			// last usage object wholesale) sees both prompt and completion, not
			// whichever single field this event happened to carry.
			finalUsage = mergeStreamUsage(finalUsage, *u)
			chunk.Usage = finalUsage
		}
		if chunk.Delta != "" {
			completion.WriteString(chunk.Delta)
			// Capture the first non-empty delta's arrival as the TTFT anchor
			// (first-only; later deltas do not move it). Mirrors pumpStream.
			if firstTokenAt == nil {
				now := time.Now()
				firstTokenAt = &now
			}
		}
		if !meaningful && !chunk.Done {
			return
		}
		obj := adapter.BuildOpenAIStreamChunk(modelID, chunk)
		if obj == nil {
			return
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return
		}
		writeSSERaw(c, string(b))
	}

	// readTerminalErr reads the producer's single terminal error value (sent once
	// just before close(events)). A non-EOF error is fatal; EOF/nil is a clean end.
	readTerminalErr := func() {
		if err := <-errCh; err != nil && !errors.Is(err, io.EOF) {
			fatalErr = err
		}
	}

	c.Stream(func(w io.Writer) bool {
		if clientGone || fatalErr != nil {
			return false
		}
		select {
		case raw, ok := <-events:
			if !ok {
				// Drain the single terminal error value so the producer goroutine
				// completes; stream output already flushed.
				readTerminalErr()
				return false
			}
			process(raw)
			return true
		default:
		}
		select {
		case <-c.Request.Context().Done():
			clientGone = true
			return false
		case raw, ok := <-events:
			if !ok {
				readTerminalErr()
				return false
			}
			process(raw)
			return true
		}
	})

	// Resolve TTFT relative to the stream start (nil if no delta ever arrived).
	var firstToken *time.Duration
	if firstTokenAt != nil {
		d := firstTokenAt.Sub(streamStart)
		firstToken = &d
	}

	return finalUsage, completion.String(), firstToken, fatalErr
}

// recordUpstreamError records an adapter error onto the is_test log state using
// the SAME status/message resolution as writeUpstreamError, so the persisted
// http_status/error_message match what the client received. The upstream error
// text is captured verbatim so it is visible in the dashboard (Tech Design §3.2).
func (tc *TestChatController) recordUpstreamError(st *testLogState, err error) {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		status := ue.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		msg := ue.Message
		if msg == "" {
			msg = ue.Error()
		}
		st.fail(status, msg)
		return
	}
	st.fail(http.StatusBadGateway, "upstream response could not be parsed: "+err.Error())
}

// writeUpstreamError renders an adapter error: an *UpstreamError carries the
// upstream status (clamped into 4xx/5xx, defaulting to 502) and message; any
// other error is a generic 502 upstream failure. The error text is surfaced.
func (tc *TestChatController) writeUpstreamError(c *gin.Context, err error) {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		status := ue.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		msg := ue.Message
		if msg == "" {
			msg = ue.Error()
		}
		writeTestChatError(c, status, "upstream_error", msg)
		return
	}
	writeTestChatError(c, http.StatusBadGateway, "upstream_error", "upstream response could not be parsed: "+err.Error())
}

// resolveTestModel resolves the model id to send upstream and an optional
// non-blocking warning. Rules (Tech Design §3):
//   - explicit model: used as-is; warn if it is not in the channel's model list.
//   - omitted model: if the channel lists exactly one model, use it; otherwise
//     the model is required (returns an error string, the caller renders 400).
//
// A non-empty errMsg means "reject with 400"; warning is advisory only.
func resolveTestModel(requested string, channelModels []string) (resolved, warning, errMsg string) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		switch len(channelModels) {
		case 1:
			return channelModels[0], "", ""
		case 0:
			return "", "", "field \"model\" is required: channel has no models listed (run fetch-models or specify a model)"
		default:
			return "", "", "field \"model\" is required: channel lists multiple models, specify one"
		}
	}
	if len(channelModels) > 0 && !containsString(channelModels, requested) {
		warning = "model \"" + requested + "\" is not in this channel's listed models; sending anyway"
	}
	return requested, warning, ""
}

// buildTestUnifiedRequest assembles a UnifiedRequest from the test-chat body and
// the resolved model. It reuses the inbound OpenAI parser so content parts
// (text + image_url, base64/http) flow into image-aware ContentBlocks exactly
// like the /v1 path.
func buildTestUnifiedRequest(req testChatRequest, resolvedModel string) adapter.UnifiedRequest {
	in := adapter.OpenAIChatInbound{
		Model:       resolvedModel,
		Messages:    req.Messages,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	return adapter.ParseOpenAIRequest(in)
}

// writeTestChatError renders the OpenAI error schema {error:{message,type}} and
// aborts. test-chat always speaks the OpenAI dialect.
func writeTestChatError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}

// writeSSEComment writes an SSE comment line (": <text>") and flushes. Comments
// are ignored by SSE parsers, so they convey advisory info without polluting the
// data stream.
func writeSSEComment(c *gin.Context, text string) {
	_, _ = io.WriteString(c.Writer, ": "+text+"\n\n")
	c.Writer.Flush()
}

// parseTestChannelID extracts and validates the :id path parameter, rendering an
// OpenAI-schema 400 on failure.
func parseTestChannelID(c *gin.Context) (uint, bool) {
	raw := c.Param("id")
	var id uint64
	for _, r := range raw {
		if r < '0' || r > '9' {
			writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", "invalid channel id")
			return 0, false
		}
		id = id*10 + uint64(r-'0')
	}
	if raw == "" {
		writeTestChatError(c, http.StatusBadRequest, "invalid_request_error", "invalid channel id")
		return 0, false
	}
	return uint(id), true
}

// decodeChannelModels parses the channel's Models JSONB array into a string slice.
func decodeChannelModels(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// containsString reports whether s is present in xs.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
