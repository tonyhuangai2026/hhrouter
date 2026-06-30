package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router"
)

// serveStream runs the streaming relay. Failover happens during the connect
// phase: each candidate is attempted until one returns a 2xx streaming response;
// retryable connect/upstream failures advance to the next candidate. Once the
// upstream stream is open and the SSE headers/first bytes are flushed to the
// client, no further failover is possible — a mid-stream error terminates the
// stream and is logged. Each upstream chunk is parsed by the adapter and
// re-emitted in the inbound format; usage is accumulated and finalised at the end.
func (r *Relayer) serveStream(c *gin.Context, rc *requestContext, estPrompt int) {
	start := time.Now()

	sel, err := r.engine.SelectChannelCtx(c.Request.Context(), r.routeInput(rc, estPrompt))
	if err != nil {
		r.failNoChannel(c, rc, err, estPrompt, true, start)
		return
	}

	var (
		lastErr error
		lastAtt attempt
		resp    *http.Response
		ad      adapter.Adapter
		cancel  context.CancelFunc
		price   *model.ModelPrice
	)

	// Connect phase with failover.
	for {
		ch, nextErr := sel.Next()
		if nextErr != nil {
			break
		}
		a, ok := adapter.For(ch, r.channels)
		if !ok {
			lastErr = errors.New("no adapter for channel type " + string(ch.Type))
			continue
		}

		upstreamModel := router.UpstreamModel(ch, rc.uni.Model)
		uni := rc.uni
		uni.Model = upstreamModel
		uni.Stream = true
		lastAtt = attempt{channel: ch, upstream: upstreamModel}

		// Price gate (USD billing) BEFORE the upstream connect: a missing price is
		// a config error — reject immediately, no failover, no bytes flushed yet.
		p, reject := r.gateModelPrice(c, rc, lastAtt, sel.Rule, estPrompt, true, start)
		if reject != nil {
			reject()
			return
		}
		price = p

		ctx, cancelFn := context.WithCancel(c.Request.Context())
		req, buildErr := a.BuildRequest(ctx, uni, ch)
		if buildErr != nil {
			cancelFn()
			lastErr = buildErr
			continue
		}

		rsp, doErr := r.streamDo(req)
		if doErr != nil {
			cancelFn()
			lastErr = doErr
			continue
		}
		if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
			// Non-2xx: read the (small) error body to decide on failover.
			body, _ := io.ReadAll(io.LimitReader(rsp.Body, 8<<10))
			_ = rsp.Body.Close()
			cancelFn()
			ue := &adapter.UpstreamError{Provider: a.Name(), StatusCode: rsp.StatusCode, Message: string(bytes.TrimSpace(body))}
			lastErr = ue
			if ue.Retryable() {
				continue
			}
			// Non-retryable: surface immediately (no bytes flushed yet).
			r.failUpstream(c, rc, ue, lastAtt, estPrompt, true, start, sel.Rule)
			return
		}

		// Connected.
		resp = rsp
		ad = a
		cancel = cancelFn
		break
	}

	if resp == nil {
		r.failAllCandidates(c, rc, lastErr, lastAtt, estPrompt, true, start, sel.Rule)
		return
	}
	defer func() { _ = resp.Body.Close(); cancel() }()

	// Stream phase: headers are now committed; proxy chunk-by-chunk. The content
	// type depends on the OUTPUT format: SSE for openai/anthropic, the AWS
	// event-stream binary framing for bedrock.
	setStreamHeaders(c, rc.outFormat)

	// When the upstream speaks Anthropic AND the client wants Anthropic output, we
	// forward the SSE events verbatim (pumpAnthropicPassthrough). This preserves
	// the full multi-block framing — including tool_use blocks and input_json_delta
	// argument streaming — that the reshaping pumpStream path collapses to a single
	// text block (which is why tool calls hung). All other format combinations use
	// the transform pump.
	var (
		usage          *adapter.Usage
		completionText string
		firstToken     *time.Duration
		streamErr      error
	)
	if ad.Name() == "anthropic" && rc.outFormat == OutAnthropic {
		usage, completionText, firstToken, streamErr = r.pumpAnthropicPassthrough(c, resp, start)
	} else {
		usage, completionText, firstToken, streamErr = r.pumpStream(c, rc, ad, resp, start)
	}

	prompt, completion, total := finalizeUsage(usage, completionText, estPrompt)
	status := model.LogSuccess
	httpStatus := http.StatusOK
	errMsg := ""
	if streamErr != nil {
		status = model.LogError
		errMsg = streamErr.Error()
		if ue := upstreamHTTPStatus(streamErr); ue != 0 {
			httpStatus = ue
		}
	} else if completionText == "" && usage == nil {
		// The stream ended cleanly (messageStop/EOF) but produced no content and
		// no usage — surface a readable warning so this is distinguishable from a
		// genuine non-empty completion in the request log.
		errMsg = emptyStreamWarning
	}

	// Bill only on success: compute cost from the resolved price × actual usage
	// (incl. cache). On a mid-stream error we pass nil (no cost, no debit). Use
	// the normalized prompt/completion/cache counts so the cost matches the logged
	// tokens even when the upstream omitted prompt usage (estPrompt floor).
	var bill *billing
	if status == model.LogSuccess && price != nil {
		bu := adapter.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
		if usage != nil {
			bu.CacheReadTokens = usage.CacheReadTokens
			bu.CacheWriteTokens = usage.CacheWriteTokens
		}
		bill = &billing{price: price, usage: bu}
	}
	r.finish(rc, lastAtt, sel.Rule, status, httpStatus, errMsg, prompt, completion, total, true, time.Since(start), firstToken, bill)
}

// anthropicPassthroughEvent is a minimal view of an Anthropic streaming event,
// used by the passthrough pump to sniff usage / stop_reason / text for billing
// and logging WITHOUT reshaping the event (the bytes are forwarded verbatim).
type anthropicPassthroughEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// pumpAnthropicPassthrough forwards an Anthropic upstream SSE stream to an
// Anthropic-output client byte-for-byte, reconstructing the `event:` name from
// each payload's self-describing `type` field. Unlike pumpStream it does NOT
// reshape events into a single text block, so multi-block tool-call streams
// (tool_use + input_json_delta across several indices) round-trip intact — the
// fix for tool calling hanging. It still sniffs usage, stop_reason, text and
// mid-stream error events so billing, TTFT and request logging behave the same.
//
// Used only when BOTH the upstream and the output format are Anthropic. All
// other combinations keep using pumpStream's transform path.
func (r *Relayer) pumpAnthropicPassthrough(c *gin.Context, resp *http.Response, streamStart time.Time) (*adapter.Usage, string, *time.Duration, error) {
	events, errCh := upstreamEvents("anthropic", resp.Body)

	var (
		finalUsage   *adapter.Usage
		completion   bytes.Buffer
		fatalErr     error
		clientGone   bool
		firstTokenAt *time.Time
	)

	process := func(raw streamEvent) {
		payload := raw.Payload
		var ev anthropicPassthroughEvent
		// A malformed payload is still forwarded verbatim (the client can cope);
		// we just cannot sniff it.
		_ = json.Unmarshal(payload, &ev)

		if ev.Type == "error" {
			msg := "anthropic upstream stream error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			fatalErr = &adapter.UpstreamError{Provider: "anthropic", StatusCode: http.StatusBadGateway, Message: msg}
			// Still forward the error event so the client sees it.
			writeSSENamedRaw(c, eventNameFor(ev.Type, payload), payload)
			return
		}

		// Sniff usage from message_start (input + cache) and message_delta (output).
		if ev.Message != nil && ev.Message.Usage != nil {
			u := adapter.Usage{
				PromptTokens:     ev.Message.Usage.InputTokens,
				CacheReadTokens:  ev.Message.Usage.CacheReadInputTokens,
				CacheWriteTokens: ev.Message.Usage.CacheCreationInputTokens,
				HasUpstream:      true,
			}
			finalUsage = mergeStreamUsage(finalUsage, u)
		}
		if ev.Usage != nil {
			finalUsage = mergeStreamUsage(finalUsage, adapter.Usage{CompletionTokens: ev.Usage.OutputTokens, HasUpstream: true})
		}
		// Sniff text deltas for the TTFT anchor + completion-text fallback.
		if ev.Delta != nil && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			completion.WriteString(ev.Delta.Text)
			if firstTokenAt == nil {
				now := time.Now()
				firstTokenAt = &now
			}
		}
		// An input_json_delta (tool arguments) also counts as first token activity.
		if ev.Delta != nil && ev.Delta.Type == "input_json_delta" && firstTokenAt == nil {
			now := time.Now()
			firstTokenAt = &now
		}

		// Forward the event verbatim.
		writeSSENamedRaw(c, eventNameFor(ev.Type, payload), payload)
	}

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

	var firstToken *time.Duration
	if firstTokenAt != nil {
		d := firstTokenAt.Sub(streamStart)
		firstToken = &d
	}
	return finalUsage, completion.String(), firstToken, fatalErr
}

// eventNameFor returns the SSE `event:` name for an Anthropic payload. It prefers
// the payload's self-describing `type` field (so it matches the upstream exactly)
// and falls back to empty (no event line) only when type is absent.
func eventNameFor(typ string, _ []byte) string { return typ }

// pumpStream reads the upstream stream, converts each event to the inbound
// format, writes it via gin's c.Stream, and accumulates usage. It returns the
// final usage (may be nil if the upstream never reported it), the concatenated
// completion text (for an estimate fallback) and any fatal error.
func (r *Relayer) pumpStream(c *gin.Context, rc *requestContext, ad adapter.Adapter, resp *http.Response, streamStart time.Time) (*adapter.Usage, string, *time.Duration, error) {
	events, errCh := upstreamEvents(ad.Name(), resp.Body)

	var (
		finalUsage    *adapter.Usage
		completion    bytes.Buffer
		anthOpened    bool
		bedrockOpened bool
		finalStop     adapter.StopReason = adapter.StopUnknown
		fatalErr      error
		clientGone    bool
		firstTokenAt  *time.Time // wall-clock of the first non-empty delta (TTFT anchor)
	)

	// For Anthropic OUTPUT we must emit the message_start / content_block_start
	// framing before the first delta and message_stop at the end.
	emitAnthropicPrelude := func() {
		if anthOpened {
			return
		}
		anthOpened = true
		writeSSENamed(c, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":          "msg_" + newID(),
				"type":        "message",
				"role":        adapter.RoleAssistant,
				"model":       rc.uni.Model,
				"content":     []any{},
				"stop_reason": nil,
				"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		writeSSENamed(c, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}

	// For Bedrock OUTPUT we emit the messageStart / contentBlockStart binary
	// frames before the first delta; contentBlockStop / messageStop / metadata
	// close the stream (in the terminal block below).
	emitBedrockPrelude := func() {
		if bedrockOpened {
			return
		}
		bedrockOpened = true
		writeBedrockFrame(c, adapter.BedrockEventMessageStart, adapter.BedrockMessageStartPayload())
		writeBedrockFrame(c, "contentBlockStart", adapter.BedrockContentBlockStartPayload())
	}

	// ensurePrelude opens the per-output-format framing on the first content delta.
	ensurePrelude := func() {
		switch rc.outFormat {
		case OutAnthropic:
			emitAnthropicPrelude()
		case OutBedrock:
			emitBedrockPrelude()
		}
	}

	// process forwards a single upstream event to the client and captures any
	// usage it carries. It is used for every drained event, including the final
	// usage-bearing tail event, so end-of-stream usage is always recorded. A
	// chunk carrying a fatal UpstreamErr (e.g. a Bedrock *Exception event)
	// aborts the stream — it must NOT be treated as a skip-able malformed frame.
	process := func(ev streamEvent) {
		chunk, meaningful, perr := ad.ParseStreamChunk(ev.EventType, ev.Payload)
		if perr != nil {
			// Skip malformed chunks rather than aborting the whole stream.
			return
		}
		if chunk.UpstreamErr != nil {
			fatalErr = chunk.UpstreamErr
			return
		}
		if u := chunk.Usage; u != nil {
			// Merge (not replace): Anthropic splits prompt tokens (message_start)
			// and completion tokens (message_delta) across two events. Emit the
			// MERGED running total on the chunk so a streaming client (which keeps
			// the last usage object wholesale) sees both prompt and completion,
			// not whichever single field this event happened to carry.
			finalUsage = mergeStreamUsage(finalUsage, *u)
			chunk.Usage = finalUsage
		}
		if chunk.Delta != "" {
			completion.WriteString(chunk.Delta)
		}
		// Capture the first meaningful output — a text delta OR a tool-call delta —
		// as the TTFT anchor (first-only; later deltas do not move it). Tool-call
		// streams carry NO text deltas, so anchoring on text alone left TTFT nil and
		// the UI fell back to the (meaningless) total latency for tool-call rows.
		if firstTokenAt == nil && (chunk.Delta != "" || chunk.ToolCallDelta != nil) {
			now := time.Now()
			firstTokenAt = &now
		}
		// Track the last non-unknown stop reason so the Bedrock terminal
		// messageStop frame carries the right value (Bedrock emits stop in a
		// dedicated event, not on the delta chunks).
		if chunk.StopReason != adapter.StopUnknown {
			finalStop = chunk.StopReason
		}
		if !meaningful && !chunk.Done {
			return
		}
		r.emitChunk(c, rc, chunk, ensurePrelude)
	}

	// readTerminalErr reads the producer's single terminal error value. It is sent
	// exactly once, just before close(events), so by the time the events channel is
	// observed closed this never blocks. A non-EOF error is fatal; EOF/nil means a
	// clean end-of-stream.
	readTerminalErr := func() {
		if err := <-errCh; err != nil && !errors.Is(err, io.EOF) {
			fatalErr = err
		}
	}

	c.Stream(func(w io.Writer) bool {
		if clientGone || fatalErr != nil {
			return false
		}
		// Drain events with strict priority over the terminal signal. The producer
		// pushes ALL events (including the final usage-bearing tail event) onto the
		// buffered events channel before it writes errCh and closes events, so we
		// must fully drain events before consulting errCh. Reading errCh in the same
		// select as events would race: both can be ready at once and select would
		// pick errCh ~half the time, abandoning the buffered usage event. Instead we
		// only look at errCh once events is observed closed.
		select {
		case raw, ok := <-events:
			if !ok {
				readTerminalErr()
				return false
			}
			process(raw)
			return true
		default:
		}
		// No event buffered right now: block until the next event arrives, the
		// stream ends, or the client disconnects. We do NOT select on errCh here —
		// the events channel closing is the single end-of-stream signal, after which
		// readTerminalErr distinguishes clean EOF from a fatal error.
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

	// Emit the terminal framing for the OUTPUT format.
	switch rc.outFormat {
	case OutAnthropic:
		if !anthOpened {
			emitAnthropicPrelude()
		}
		writeSSENamed(c, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		// message_delta with usage + message_stop are emitted by emitChunk on the
		// terminal chunk; ensure message_stop closes the stream.
		writeSSENamed(c, "message_stop", map[string]any{"type": "message_stop"})
	case OutBedrock:
		if !bedrockOpened {
			emitBedrockPrelude()
		}
		writeBedrockFrame(c, adapter.BedrockEventContentBlockStop, adapter.BedrockContentBlockStopPayload())
		writeBedrockFrame(c, adapter.BedrockEventMessageStop, adapter.BedrockMessageStopPayload(finalStop))
		// metadata carries terminal usage (Bedrock's usage event), emitted
		// explicitly here rather than via the per-chunk usage path.
		var mu adapter.Usage
		if finalUsage != nil {
			mu = *finalUsage
		}
		writeBedrockFrame(c, adapter.BedrockEventMetadata, adapter.BedrockMetadataPayload(mu))
	default: // OutOpenAI
		writeSSERaw(c, "[DONE]")
	}

	// Resolve TTFT relative to the stream start (nil if no delta ever arrived,
	// e.g. an empty or pre-first-token-errored stream).
	var firstToken *time.Duration
	if firstTokenAt != nil {
		d := firstTokenAt.Sub(streamStart)
		firstToken = &d
	}

	return finalUsage, completion.String(), firstToken, fatalErr
}

// emitChunk converts a unified StreamChunk into the OUTPUT format's event(s) and
// writes them to the client. ensurePrelude opens the per-format framing before
// the first content delta.
func (r *Relayer) emitChunk(c *gin.Context, rc *requestContext, chunk adapter.StreamChunk, ensurePrelude func()) {
	switch rc.outFormat {
	case OutAnthropic:
		event, payload, ok := adapter.BuildAnthropicStreamEvent(chunk)
		if !ok {
			return
		}
		// Ensure prelude framing exists before the first content delta.
		if event == "content_block_delta" {
			ensurePrelude()
		}
		writeSSENamed(c, event, payload)
		return
	case OutBedrock:
		// Only content deltas produce a per-chunk frame; stop/usage are emitted by
		// the terminal block as contentBlockStop/messageStop/metadata.
		eventType, payload, ok := adapter.BuildBedrockStreamEvent(chunk)
		if !ok {
			return
		}
		ensurePrelude()
		writeBedrockFrame(c, eventType, payload)
		return
	default: // OutOpenAI
		// A single chat.completion.chunk object (the [DONE] sentinel is emitted by
		// the caller).
		obj := adapter.BuildOpenAIStreamChunk(rc.uni.Model, chunk)
		if obj == nil {
			return
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return
		}
		writeSSERaw(c, string(b))
	}
}

// emptyStreamWarning is logged (in error_message) when a stream ends cleanly but
// carried no text and no usage, distinguishing it from a real completion.
const emptyStreamWarning = "upstream stream produced no content"

// upstreamHTTPStatus returns the HTTP status carried by a fatal mid-stream
// *UpstreamError (clamped into 4xx/5xx, defaulting to 502), or 0 if err is not
// an *UpstreamError. Used to record a meaningful http_status in the request log
// for a mid-stream upstream failure (the client status stays 200 since headers
// are already committed once streaming starts).
func upstreamHTTPStatus(err error) int {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		s := ue.StatusCode
		if s < 400 || s > 599 {
			return http.StatusBadGateway
		}
		return s
	}
	return 0
}

// finalizeUsage resolves the final token counts for a stream: it prefers the
// upstream-reported usage and falls back to an estimate from the accumulated
// completion text (with the pre-flight prompt estimate as the prompt floor).
func finalizeUsage(usage *adapter.Usage, completionText string, estPrompt int) (prompt, completion, total int) {
	if usage != nil {
		return usageTokens(*usage, estPrompt)
	}
	comp := estimateTextTokens(completionText)
	return estPrompt, comp, estPrompt + comp
}

// estimateTextTokens mirrors the adapter's ~4-chars-per-token heuristic for the
// stream fallback (the adapter helper is unexported).
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// setSSEHeaders configures the response for Server-Sent Events.
func setSSEHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()
}

// setStreamHeaders configures the streaming response Content-Type for the output
// format: SSE (text/event-stream) for openai/anthropic, the AWS event-stream
// binary framing (application/vnd.amazon.eventstream) for bedrock. The other
// streaming headers (no-cache / keep-alive / no-buffering) apply to both.
func setStreamHeaders(c *gin.Context, out OutputFormat) {
	h := c.Writer.Header()
	if out == OutBedrock {
		h.Set("Content-Type", "application/vnd.amazon.eventstream")
	} else {
		h.Set("Content-Type", "text/event-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()
}

// writeBedrockFrame JSON-encodes payload and writes one AWS event-stream binary
// frame (via the adapter encoder) to the client, then flushes. Used to render a
// streamed response as real Bedrock ConverseStream frames.
func writeBedrockFrame(c *gin.Context, eventType string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.Writer.Write(adapter.EncodeBedrockFrame(eventType, b))
	c.Writer.Flush()
}

// writeSSERaw writes a single `data: <payload>` SSE line and flushes.
func writeSSERaw(c *gin.Context, payload string) {
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
	c.Writer.Flush()
}

// writeSSENamed writes a named SSE event (`event: <name>` + `data: <json>`),
// used for the Anthropic streaming protocol, and flushes.
func writeSSENamed(c *gin.Context, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, b)
	c.Writer.Flush()
}

// writeSSENamedRaw writes a named SSE event with a pre-serialized JSON payload,
// preserving the upstream bytes exactly (used by the Anthropic passthrough so
// tool_use blocks and partial_json deltas round-trip losslessly).
func writeSSENamedRaw(c *gin.Context, event string, payload []byte) {
	if event != "" {
		_, _ = fmt.Fprintf(c.Writer, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
	c.Writer.Flush()
}

// streamEvent is one de-framed upstream event handed to the pump. EventType is
// the event discriminator: for Bedrock ConverseStream it is the value of the AWS
// event-stream `:event-type` header (e.g. "contentBlockDelta", "messageStop",
// "metadata", "...Exception"); for plain-SSE upstreams (OpenAI) there is no such
// header and EventType is "". Payload is the event body: for Bedrock the UNWRAPPED
// inner JSON of the event, for OpenAI the bytes after the SSE "data: " prefix.
type streamEvent struct {
	EventType string
	Payload   []byte
}

// upstreamEvents reads the upstream response body and emits per-event
// streamEvent values on the returned channel: for OpenAI it strips the SSE
// "data: " prefix (skipping comments/blank lines and the [DONE] sentinel, which
// the relay emits itself) and leaves EventType empty; for Bedrock it de-frames
// the AWS vnd.amazon.eventstream binary framing, extracting each frame's
// `:event-type` header and its (unwrapped) JSON payload. The error channel
// receives a terminal error (or nil at clean EOF) exactly once when the events
// channel closes.
func upstreamEvents(provider string, body io.Reader) (<-chan streamEvent, <-chan error) {
	events := make(chan streamEvent, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(events)
		var err error
		if provider == "bedrock" {
			err = readEventStream(body, events)
		} else {
			// openai AND anthropic are plain-SSE upstreams (data: payloads, no
			// AWS event-stream framing). Only bedrock needs binary deframing.
			err = readSSE(body, events)
		}
		errCh <- err
		close(errCh)
	}()

	return events, errCh
}

// mergeStreamUsage folds a chunk's usage into the running stream total, keeping
// non-zero fields rather than overwriting wholesale. This matters for upstreams
// that split usage across multiple events: Anthropic reports input_tokens on
// message_start and output_tokens on message_delta, so a plain replace would
// drop the prompt count (the later message_delta has prompt=0). OpenAI/Bedrock
// report both together, so the merge is a no-op for them. Returns the updated
// pointer (allocating on first use).
func mergeStreamUsage(prev *adapter.Usage, next adapter.Usage) *adapter.Usage {
	if prev == nil {
		cp := next
		return &cp
	}
	if next.PromptTokens != 0 {
		prev.PromptTokens = next.PromptTokens
	}
	if next.CompletionTokens != 0 {
		prev.CompletionTokens = next.CompletionTokens
	}
	if next.TotalTokens != 0 {
		prev.TotalTokens = next.TotalTokens
	}
	// Cache buckets: Anthropic reports these on message_start (with prompt) and
	// not again on message_delta, so non-zero-wins preserves them across events.
	if next.CacheReadTokens != 0 {
		prev.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheWriteTokens != 0 {
		prev.CacheWriteTokens = next.CacheWriteTokens
	}
	if next.HasUpstream {
		prev.HasUpstream = true
	}
	// Recompute total from parts when the upstream didn't give an explicit one.
	if prev.TotalTokens == 0 {
		prev.TotalTokens = prev.PromptTokens + prev.CompletionTokens
	}
	return prev
}

// readSSE parses a plain SSE stream, sending each event's data payload on out
// (with an empty EventType — SSE has no event-stream discriminator header). It
// skips the [DONE] sentinel (the relay emits its own terminator).
func readSSE(body io.Reader, out chan<- streamEvent) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					payload := make([]byte, len(data))
					copy(payload, data)
					out <- streamEvent{Payload: payload}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readEventStream de-frames the AWS event-stream (vnd.amazon.eventstream) wire
// format used by Bedrock ConverseStream and sends each event's `:event-type`
// header value + (unwrapped) JSON payload on out. Each frame is:
//
//	[4B total length][4B headers length][4B prelude CRC]
//	[headers ...][payload ...][4B message CRC]
//
// We read the prelude to size the frame, parse the headers section to extract
// the `:event-type` discriminator (the event kind — AWS does NOT wrap it into the
// payload), and forward {eventType, payload} which adapter.ParseStreamChunk
// dispatches on. Pre-fix this function discarded the headers and forwarded only
// the payload, so the unwrapped payloads had no discriminator and every event was
// dropped (the 200-empty/0-token root cause).
func readEventStream(body io.Reader, out chan<- streamEvent) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	prelude := make([]byte, 12)
	for {
		if _, err := io.ReadFull(reader, prelude); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		totalLen := binary.BigEndian.Uint32(prelude[0:4])
		headersLen := binary.BigEndian.Uint32(prelude[4:8])
		if totalLen < 16 || headersLen > totalLen {
			return fmt.Errorf("bedrock event-stream: malformed frame lengths total=%d headers=%d", totalLen, headersLen)
		}
		// Remaining bytes after the 12-byte prelude already consumed.
		rest := make([]byte, totalLen-12)
		if _, err := io.ReadFull(reader, rest); err != nil {
			return err
		}
		// rest = [headers (headersLen)][payload][message CRC (4)].
		if int(headersLen)+4 > len(rest) {
			return fmt.Errorf("bedrock event-stream: payload bounds exceed frame")
		}
		eventType := eventTypeFromHeaders(rest[:headersLen])
		payload := rest[headersLen : len(rest)-4]
		if len(payload) > 0 || eventType != "" {
			cp := make([]byte, len(payload))
			copy(cp, payload)
			out <- streamEvent{EventType: eventType, Payload: cp}
		}
	}
}

// eventTypeFromHeaders parses the AWS event-stream headers section and returns
// the value of the `:event-type` header (empty if absent / unparseable). Each
// header is encoded as:
//
//	name-len (1 byte) | name (name-len bytes) | value-type (1 byte) | value...
//
// where the value encoding depends on value-type. We need only the string type
// (7: 2-byte big-endian length, then that many bytes); all other value-types are
// skipped by their fixed/length-prefixed size so multi-header frames parse
// correctly. Bedrock encodes `:event-type` (and `:content-type`/`:message-type`)
// as string headers, so this reliably recovers the discriminator.
func eventTypeFromHeaders(h []byte) string {
	const eventTypeHeader = ":event-type"
	i := 0
	for i < len(h) {
		nameLen := int(h[i])
		i++
		if i+nameLen > len(h) {
			return ""
		}
		name := string(h[i : i+nameLen])
		i += nameLen
		if i >= len(h) {
			return ""
		}
		valueType := h[i]
		i++
		// Decode/skip the value per its wire type. Sizes per the AWS
		// event-stream spec (HeaderValue): 0/1 bool (no bytes), 2 byte (1),
		// 3 short (2), 4 int (4), 5 long (8), 6 byte-array & 7 string (2-byte
		// length prefix + N), 8 timestamp (8), 9 uuid (16).
		switch valueType {
		case 0, 1: // bool true/false — value is the type itself, no extra bytes
			// nothing to consume
		case 2: // byte
			i++
		case 3: // short
			i += 2
		case 4: // int32
			i += 4
		case 5: // int64
			i += 8
		case 6, 7: // byte array / string — 2-byte length prefix then bytes
			if i+2 > len(h) {
				return ""
			}
			vlen := int(binary.BigEndian.Uint16(h[i : i+2]))
			i += 2
			if i+vlen > len(h) {
				return ""
			}
			if valueType == 7 && name == eventTypeHeader {
				return string(h[i : i+vlen])
			}
			i += vlen
		case 8: // timestamp (int64 ms)
			i += 8
		case 9: // uuid
			i += 16
		default:
			// Unknown value-type: we cannot know its length, so stop parsing.
			return ""
		}
	}
	return ""
}
