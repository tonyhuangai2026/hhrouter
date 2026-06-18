package relay

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router"
	"github.com/agent-router/server/internal/service"
)

// defaultUpstreamTimeout bounds a single non-streaming upstream attempt.
const defaultUpstreamTimeout = 120 * time.Second

// Relayer drives a relay request end-to-end: it holds the wired dependencies and
// exposes the HTTP-call + failover + accounting machinery shared by the
// streaming and non-streaming paths. It is safe for concurrent use.
type Relayer struct {
	engine   *router.Engine
	channels *service.ChannelService // also the adapter.Decryptor
	quota    *service.QuotaService
	logs     *service.LogService
	pricing  *service.PricingService
	db       *gorm.DB
	httpDo   func(*http.Request) (*http.Response, error)
	streamDo func(*http.Request) (*http.Response, error)
}

// NewRelayer constructs a Relayer from the wired services. The same
// ChannelService doubles as the adapter Decryptor.
func NewRelayer(engine *router.Engine, channels *service.ChannelService, quota *service.QuotaService, logs *service.LogService, pricing *service.PricingService, db *gorm.DB) *Relayer {
	nonStream := &http.Client{Timeout: defaultUpstreamTimeout}
	// Streaming uses no overall client timeout (the body is read incrementally);
	// cancellation is driven by the request context instead.
	stream := &http.Client{}
	return &Relayer{
		engine:   engine,
		channels: channels,
		quota:    quota,
		logs:     logs,
		pricing:  pricing,
		db:       db,
		httpDo:   nonStream.Do,
		streamDo: stream.Do,
	}
}

// requestContext carries the parsed inbound request plus the authenticated
// identity through the relay pipeline.
type requestContext struct {
	// format is the TRUE inbound dialect (from the endpoint). It governs request
	// parsing and is what request_log.inbound_format records — it is NOT changed
	// by a key's output-format override.
	format InboundFormat
	// outFormat is the dialect the RESPONSE is rendered in (body, stream frames,
	// relay-internal errors). Defaults to mirror format; a key's OutputFormat
	// override (openai/anthropic/bedrock) replaces it. See outFormatFor.
	outFormat OutputFormat
	uni       adapter.UnifiedRequest
	token     *model.Token
	user      *model.User
}

// attempt records the outcome of one candidate-channel attempt for logging.
type attempt struct {
	channel  *model.Channel
	upstream string // resolved upstream model id
}

// HandleChatCompletions handles POST /v1/chat/completions (OpenAI inbound).
func (r *Relayer) HandleChatCompletions(c *gin.Context) {
	var in adapter.OpenAIChatInbound
	if err := c.ShouldBindJSON(&in); err != nil {
		WriteClassError(c, FormatOpenAI, http.StatusBadRequest, ClassInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if in.Model == "" {
		WriteClassError(c, FormatOpenAI, http.StatusBadRequest, ClassInvalidRequest, "field \"model\" is required")
		return
	}
	uni := adapter.ParseOpenAIRequest(in)
	r.handle(c, FormatOpenAI, uni)
}

// HandleMessages handles POST /v1/messages (Anthropic Messages inbound).
func (r *Relayer) HandleMessages(c *gin.Context) {
	var in adapter.AnthropicInbound
	if err := c.ShouldBindJSON(&in); err != nil {
		WriteClassError(c, FormatAnthropic, http.StatusBadRequest, ClassInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if in.Model == "" {
		WriteClassError(c, FormatAnthropic, http.StatusBadRequest, ClassInvalidRequest, "field \"model\" is required")
		return
	}
	uni := adapter.ParseAnthropicRequest(in)
	r.handle(c, FormatAnthropic, uni)
}

// handle is the shared entry that resolves identity, runs the precise pre-flight
// quota admission, and dispatches to the streaming or non-streaming path.
func (r *Relayer) handle(c *gin.Context, format InboundFormat, uni adapter.UnifiedRequest) {
	tok, ok := tokenFromCtx(c)
	if !ok {
		WriteClassError(c, format, http.StatusUnauthorized, ClassAuthentication, "authentication required")
		return
	}
	user, ok := userFromCtx(c)
	if !ok {
		WriteClassError(c, format, http.StatusUnauthorized, ClassAuthentication, "authentication required")
		return
	}

	// Enforce the token's allowed-model restriction (empty = no restriction).
	if !modelAllowed(tok, uni.Model) {
		WriteClassError(c, format, http.StatusForbidden, ClassPermission,
			"model \""+uni.Model+"\" is not allowed for this API key")
		return
	}

	estPrompt := adapter.EstimatePromptTokens(uni)

	// NOTE: the precise pre-flight admission is now USD-based and price-dependent,
	// so it cannot run here (no channel/model price is resolved until after
	// SelectChannel). It is performed inside serveNonStream/serveStream once the
	// channel is chosen and its (channel, model) price is looked up. The cheap
	// "already exhausted" guard (remaining == 0) still runs in middleware.Quota
	// before the body is parsed.

	rc := &requestContext{
		format:    format,
		outFormat: outFormatFor(format, tok.OutputFormat), // key override or mirror endpoint
		uni:       uni,
		token:     tok,
		user:      user,
	}
	if uni.Stream {
		r.serveStream(c, rc, estPrompt)
		return
	}
	r.serveNonStream(c, rc, estPrompt)
}

// gateModelPrice enforces USD billing admission for a chosen candidate channel
// BEFORE any upstream call (Tech Design §4.1):
//
//  1. Look up the (channel, upstreamModel) price. A missing/incomplete price
//     (ErrPriceNotConfigured) is an operator-configuration problem, NOT an
//     upstream fault: it returns a non-nil rejecter that writes a 400 +
//     error-logs, and the caller must STOP (no failover to other channels).
//  2. USD pre-flight: estimate the cost of the prompt at the model's input price
//     and reject with 402 when it exceeds the remaining USD balance.
//
// On success it returns the resolved price and reject == nil. On any gate
// failure it returns reject != nil — the caller invokes it (it writes the client
// error + the error request_log) and returns without trying another candidate.
func (r *Relayer) gateModelPrice(c *gin.Context, rc *requestContext, att attempt, rule *model.RoutingRule, estPrompt int, stream bool, start time.Time) (price *model.ModelPrice, reject func()) {
	price, err := r.pricing.Lookup(att.channel.ID, att.upstream)
	if err != nil {
		if errors.Is(err, service.ErrPriceNotConfigured) {
			msg := "model \"" + att.upstream + "\" has no price configured on channel \"" + att.channel.Name + "\"; an administrator must configure its price before it can be requested"
			return nil, func() {
				WriteOutError(c, rc.outFormat, http.StatusBadRequest, ClassInvalidRequest, msg)
				r.finish(rc, att, rule, model.LogError, http.StatusBadRequest, msg, estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
			}
		}
		// Unexpected lookup error: surface as internal, no failover.
		return nil, func() {
			WriteOutError(c, rc.outFormat, http.StatusInternalServerError, ClassInternal, "could not look up model price")
			r.finish(rc, att, rule, model.LogError, http.StatusInternalServerError, "price lookup: "+err.Error(), estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
		}
	}

	// USD pre-flight: estimated input cost vs remaining USD balance.
	estCost := r.pricing.Cost(price, adapter.Usage{PromptTokens: estPrompt})
	remaining, qerr := r.quota.CheckRemaining(c.Request.Context(), rc.token.ID, rc.user.ID, estCost)
	if qerr != nil {
		return nil, func() {
			WriteOutError(c, rc.outFormat, http.StatusInternalServerError, ClassInternal, "could not check quota")
			r.finish(rc, att, rule, model.LogError, http.StatusInternalServerError, "quota check: "+qerr.Error(), estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
		}
	}
	if remaining >= 0 && remaining < estCost {
		msg := "insufficient balance: estimated cost " + formatMicroUSD(estCost) + " exceeds remaining " + formatMicroUSD(remaining)
		return nil, func() {
			WriteOutError(c, rc.outFormat, http.StatusPaymentRequired, ClassQuota, msg)
			r.finish(rc, att, rule, model.LogError, http.StatusPaymentRequired, msg, estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
		}
	}
	return price, nil
}

// formatMicroUSD renders a micro-USD amount as a "$X.XXXX" string for error
// messages (1 USD = 1_000_000 micro-USD; 4 decimal places).
func formatMicroUSD(micro int64) string {
	return "$" + strconv.FormatFloat(float64(micro)/1e6, 'f', 4, 64)
}

// serveNonStream runs the non-streaming relay: select candidates, attempt each
// (failing over on retryable upstream errors), adapt the response back to the
// inbound format, then log and consume quota.
func (r *Relayer) serveNonStream(c *gin.Context, rc *requestContext, estPrompt int) {
	start := time.Now()

	sel, err := r.engine.SelectChannel(rc.token.Group, rc.uni.Model, estPrompt)
	if err != nil {
		r.failNoChannel(c, rc, err, estPrompt, false, start)
		return
	}

	var lastErr error
	var lastAtt attempt
	for {
		ch, nextErr := sel.Next()
		if nextErr != nil {
			break
		}

		ad, ok := adapter.For(ch, r.channels)
		if !ok {
			lastErr = errors.New("no adapter for channel type " + string(ch.Type))
			continue
		}

		// Resolve the upstream model id (model_mapping) for this channel.
		upstreamModel := router.UpstreamModel(ch, rc.uni.Model)
		uni := rc.uni
		uni.Model = upstreamModel
		lastAtt = attempt{channel: ch, upstream: upstreamModel}

		// Price gate (USD billing): the (channel, model) MUST have a configured
		// price. A missing price is a config error — reject immediately, do NOT
		// fail over to another channel.
		price, reject := r.gateModelPrice(c, rc, lastAtt, sel.Rule, estPrompt, false, start)
		if reject != nil {
			reject()
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), defaultUpstreamTimeout)
		req, buildErr := ad.BuildRequest(ctx, uni, ch)
		if buildErr != nil {
			cancel()
			lastErr = buildErr
			continue
		}

		resp, doErr := r.httpDo(req)
		if doErr != nil {
			cancel()
			// Network-level failures are retryable: try the next candidate.
			lastErr = doErr
			continue
		}

		uniResp, usage, parseErr := ad.ParseResponse(resp)
		_ = resp.Body.Close()
		cancel()
		if parseErr != nil {
			lastErr = parseErr
			var ue *adapter.UpstreamError
			if errors.As(parseErr, &ue) && !ue.Retryable() {
				// Non-retryable upstream error (e.g. 4xx): surface immediately.
				r.failUpstream(c, rc, ue, lastAtt, estPrompt, false, start, sel.Rule)
				return
			}
			// Retryable: fail over to the next candidate.
			continue
		}

		// Success: adapt and write the response in the key's OUTPUT format, then account.
		uniResp.Model = rc.uni.Model // echo the external model name to the client
		body := buildResponse(rc.outFormat, uniResp)
		c.JSON(http.StatusOK, body)

		prompt, completion, total := usageTokens(usage, estPrompt)
		r.finish(rc, lastAtt, sel.Rule, model.LogSuccess, http.StatusOK, "",
			prompt, completion, total, false, time.Since(start), nil, &billing{price: price, usage: usage})
		return
	}

	// All candidates exhausted.
	r.failAllCandidates(c, rc, lastErr, lastAtt, estPrompt, false, start, sel.Rule)
}

// failNoChannel handles the case where the engine found no candidate channel.
func (r *Relayer) failNoChannel(c *gin.Context, rc *requestContext, err error, estPrompt int, stream bool, start time.Time) {
	msg := "no upstream channel can serve model \"" + rc.uni.Model + "\""
	if !errors.Is(err, router.ErrNoCandidate) {
		msg = "routing failed: " + err.Error()
	}
	WriteOutError(c, rc.outFormat, http.StatusBadGateway, ClassUpstream, msg)
	r.finish(rc, attempt{}, nil, model.LogError, http.StatusBadGateway, msg, estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
}

// failUpstream handles a non-retryable upstream error: it propagates the upstream
// status (mapped into the inbound error schema) and logs the failure.
func (r *Relayer) failUpstream(c *gin.Context, rc *requestContext, ue *adapter.UpstreamError, att attempt, estPrompt int, stream bool, start time.Time, rule *model.RoutingRule) {
	status := ue.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	WriteOutError(c, rc.outFormat, status, ClassUpstream, ue.Message)
	r.finish(rc, att, rule, model.LogError, status, ue.Error(), estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
}

// failAllCandidates handles exhaustion of the failover budget.
func (r *Relayer) failAllCandidates(c *gin.Context, rc *requestContext, lastErr error, att attempt, estPrompt int, stream bool, start time.Time, rule *model.RoutingRule) {
	msg := "all candidate upstream channels failed"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	WriteOutError(c, rc.outFormat, http.StatusBadGateway, ClassUpstream, msg)
	r.finish(rc, att, rule, model.LogError, http.StatusBadGateway, msg, estPrompt, 0, estPrompt, stream, time.Since(start), nil, nil)
}

// billing carries the price + actual usage needed to compute and record a
// request's USD cost in finish(). It is non-nil only on the success path (where
// a price was resolved by the gate); error paths pass nil so the logged cost
// stays NULL and no quota is debited.
type billing struct {
	price *model.ModelPrice
	usage adapter.Usage // full usage incl. cache tokens
}

// finish writes the request_log row and consumes quota for the request. It is
// called exactly once per relay request (success or terminal failure). On
// success it computes the request's micro-USD cost from bill.price × bill.usage,
// records it (+ cache token columns) on the log, and debits that cost from the
// USD quota; failed requests (bill == nil) still log but record no cost and do
// not debit the budget.
func (r *Relayer) finish(rc *requestContext, att attempt, rule *model.RoutingRule, status model.LogStatus, httpStatus int, errMsg string, prompt, completion, total int, stream bool, latency time.Duration, firstToken *time.Duration, bill *billing) {
	var channelID uint
	upstreamModel := rc.uni.Model
	if att.channel != nil {
		channelID = att.channel.ID
		if att.upstream != "" {
			upstreamModel = att.upstream
		}
	}
	var ruleID *uint
	if rule != nil {
		id := rule.ID
		ruleID = &id
	}

	// Production /v1 traffic is always keyed by a downstream token; write a
	// non-nil token_id (the column is *uint to allow test-chat to write NULL).
	tokenID := rc.token.ID

	log := &model.RequestLog{
		UserID:           rc.user.ID,
		TokenID:          &tokenID,
		ChannelID:        channelID,
		RuleID:           ruleID,
		Model:            rc.uni.Model,
		UpstreamModel:    upstreamModel,
		InboundFormat:    rc.format,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Status:           status,
		HTTPStatus:       httpStatus,
		ErrorMessage:     errMsg,
		LatencyMs:        int(latency.Milliseconds()),
		IsStream:         stream,
	}
	if firstToken != nil {
		ms := int(firstToken.Milliseconds())
		log.FirstTokenMs = &ms
	}

	// USD cost: computed from the resolved price × actual usage (incl. cache).
	// bill is non-nil only on the success path; record the cost + cache token
	// columns on the log and debit the USD quota.
	var cost int64
	if bill != nil {
		cr := bill.usage.CacheReadTokens
		cw := bill.usage.CacheWriteTokens
		log.CacheReadTokens = &cr
		log.CacheWriteTokens = &cw
		cost = r.pricing.Cost(bill.price, bill.usage)
		log.CostMicroUSD = &cost
	}
	_ = r.logs.Write(log)

	if status == model.LogSuccess && cost > 0 {
		_ = r.quota.Consume(context.Background(), rc.token.ID, rc.user.ID, cost)
	}
}

// buildResponse renders a UnifiedResponse into the OUTPUT format's response body
// and stamps an id/created field where the format expects one. The output format
// is the key's pinned format (or the mirrored inbound dialect by default).
func buildResponse(out OutputFormat, resp adapter.UnifiedResponse) any {
	switch out {
	case OutBedrock:
		// Bedrock Converse responses carry no id/created field.
		return adapter.BuildBedrockResponse(resp)
	case OutAnthropic:
		body := adapter.BuildAnthropicResponse(resp)
		body["id"] = "msg_" + newID()
		return body
	default: // OutOpenAI
		body := adapter.BuildOpenAIResponse(resp)
		body["id"] = "chatcmpl-" + newID()
		body["created"] = time.Now().Unix()
		return body
	}
}

// modelAllowed reports whether the token's allowed-model list (if any) permits
// the requested model. An empty/absent list means "no restriction".
func modelAllowed(tok *model.Token, requested string) bool {
	allowed := decodeAllowedModels(tok)
	if len(allowed) == 0 {
		return true
	}
	for _, m := range allowed {
		if m == requested {
			return true
		}
	}
	return false
}

// tokenFromCtx / userFromCtx pull the identity injected by RelayAuth without
// importing the middleware package (avoids an import cycle: middleware imports
// relay). The values are stored under the same string keys.
func tokenFromCtx(c *gin.Context) (*model.Token, bool) {
	v, ok := c.Get("relay_token")
	if !ok {
		return nil, false
	}
	t, ok := v.(*model.Token)
	return t, ok
}

func userFromCtx(c *gin.Context) (*model.User, bool) {
	v, ok := c.Get("relay_user")
	if !ok {
		return nil, false
	}
	u, ok := v.(*model.User)
	return u, ok
}
