package relay

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// stream_bedrock_test.go is the WHITE-BOX (package relay) regression for the
// Bedrock ConverseStream de-framing + dispatch fix. Its defining property: the
// mock's REAL AWS event-stream frame bytes are fed through the PRODUCTION
// readEventStream / upstreamEvents de-framer (NOT a test-private re-implementation
// — the prior bedrock_mock_test.go deframeEventStream bypass that masked the bug
// has been removed). Encoding-and-decoding are thereby cross-checked by the SAME
// production de-framer that runs in prod: the encoder here puts the discriminator
// ONLY in the :event-type header with the real AWS binary header layout, and the
// production reader must recover it.

// ---- real AWS event-stream frame encoder (UNWRAPPED payload) ----------------
//
// Message layout:
//
//	[prelude: totalLen(4) | headersLen(4) | preludeCRC32(4)]
//	[headers...][payload...][messageCRC32(4)]
//
// Each header: nameLen(1) | name | valueType(1) | <value>. For a string value
// (valueType=7): valueLen(2) | valueBytes. All multi-byte ints are big-endian.
// preludeCRC32 covers the first 8 bytes; messageCRC32 covers everything from the
// start through the end of the payload. The discriminator lives ONLY in the
// :event-type header — the payload is the UNWRAPPED inner JSON, exactly as AWS
// ConverseStream sends it.

func bedrockFrame(eventType string, payload string) []byte {
	headers := encodeBedrockHeaders(map[string]string{
		":event-type":   eventType,
		":content-type": "application/json",
		":message-type": "event",
	})
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

func encodeBedrockHeaders(hs map[string]string) []byte {
	var out []byte
	for name, val := range hs {
		out = append(out, byte(len(name)))
		out = append(out, name...)
		out = append(out, 7) // value type 7 == string
		var vl [2]byte
		binary.BigEndian.PutUint16(vl[:], uint16(len(val)))
		out = append(out, vl[:]...)
		out = append(out, val...)
	}
	return out
}

// drainEventStream runs a wire byte slice through the PRODUCTION readEventStream
// de-framer and returns the (eventType, payload) of every emitted event. This is
// the exact production path — no private bypass.
func drainEventStream(t *testing.T, wire []byte) []streamEvent {
	t.Helper()
	out := make(chan streamEvent, 64)
	errc := make(chan error, 1)
	go func() { errc <- readEventStream(bytes.NewReader(wire), out); close(out) }()

	var got []streamEvent
	for ev := range out {
		// copy payload so the slice is independent of the channel buffer
		cp := append([]byte(nil), ev.Payload...)
		got = append(got, streamEvent{EventType: ev.EventType, Payload: cp})
	}
	if err := <-errc; err != nil {
		t.Fatalf("readEventStream: %v", err)
	}
	return got
}

// TestReadEventStream_ExtractsEventTypeAndUnwrappedPayload is B3 regression (a):
// real frames → production readEventStream → correct :event-type + UNWRAPPED
// payload. Pre-fix readEventStream discarded the headers and emitted only the
// payload, so eventType would be "" for every event (the root cause).
func TestReadEventStream_ExtractsEventTypeAndUnwrappedPayload(t *testing.T) {
	var wire []byte
	wire = append(wire, bedrockFrame("messageStart", `{"role":"assistant"}`)...)
	wire = append(wire, bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hi"}}`)...)
	wire = append(wire, bedrockFrame("metadata", `{"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`)...)

	got := drainEventStream(t, wire)
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
	want := []struct {
		et      string
		payload string
	}{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hi"}}`},
		{"metadata", `{"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`},
	}
	for i, w := range want {
		if got[i].EventType != w.et {
			t.Errorf("event[%d] type = %q, want %q", i, got[i].EventType, w.et)
		}
		if string(got[i].Payload) != w.payload {
			t.Errorf("event[%d] payload = %q, want %q (must be UNWRAPPED)", i, got[i].Payload, w.payload)
		}
		// And the payload must NOT be outer-wrapped under the event-type key.
		if bytes.Contains(got[i].Payload, []byte(`"`+w.et+`":`)) {
			t.Errorf("event[%d] payload is outer-wrapped under %q — real AWS does not wrap", i, w.et)
		}
	}
}

// TestEventTypeFromHeaders_MultiHeaderAndValueTypes proves the header parser
// extracts :event-type when it is not the first header and when other headers use
// non-string value-types (which must be skipped by their correct width).
func TestEventTypeFromHeaders_MultiHeaderAndValueTypes(t *testing.T) {
	// Build a header block: a bool header, an int32 header, then the string
	// :event-type — exercising the skip logic for non-string value-types.
	var h []byte
	put := func(b ...byte) { h = append(h, b...) }
	// header 1: "b" = bool true (value-type 0, no value bytes)
	put(byte(len("b")))
	put('b')
	put(0)
	// header 2: "i" = int32 (value-type 4, 4 value bytes)
	put(byte(len("i")))
	put('i')
	put(4)
	put(0, 0, 0, 42)
	// header 3: ":event-type" = string "metadata" (value-type 7)
	name := ":event-type"
	val := "metadata"
	put(byte(len(name)))
	h = append(h, name...)
	put(7)
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(val)))
	h = append(h, vl[:]...)
	h = append(h, val...)

	if got := eventTypeFromHeaders(h); got != "metadata" {
		t.Fatalf("eventTypeFromHeaders = %q, want metadata (multi-header + skipped value-types)", got)
	}
}

// TestBedrockStream_ProductionDeframe_AccumulatesText is B3 regression (b),
// end-to-end at the relay layer: a mock ConverseStream emits real AWS frames
// (messageStart / contentBlockDelta×N / contentBlockStop / messageStop /
// metadata{usage}) with UNWRAPPED payloads and the discriminator only in the
// :event-type header. The bytes flow through the PRODUCTION upstreamEvents
// de-framer, and the BedrockAdapter dispatches on the extracted eventType. The
// deltas must accumulate to the full non-empty text, the stop reason surfaces,
// and metadata.usage is parsed.
func TestBedrockStream_ProductionDeframe_AccumulatesText(t *testing.T) {
	deltas := []string{"Hel", "lo, ", "world"}
	const wantText = "Hello, world"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		write := func(b []byte) { _, _ = w.Write(b) }
		write(bedrockFrame("messageStart", `{"role":"assistant"}`))
		for _, d := range deltas {
			jb, _ := json.Marshal(d)
			write(bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":`+string(jb)+`}}`))
		}
		write(bedrockFrame("contentBlockStop", `{"contentBlockIndex":0}`))
		write(bedrockFrame("messageStop", `{"stopReason":"end_turn"}`))
		write(bedrockFrame("metadata", `{"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET mock: %v", err)
	}
	defer resp.Body.Close()

	// Production de-framer + production adapter dispatch.
	ad := adapter.NewBedrockAdapter(stubDecryptorBR{})
	events, errCh := upstreamEvents(ad.Name(), resp.Body)

	var (
		acc        []byte
		gotStop    adapter.StopReason
		gotUsage   *adapter.Usage
		sawDone    bool
		deltaCount int
	)
	for ev := range events {
		c, ok, perr := ad.ParseStreamChunk(ev.EventType, ev.Payload)
		if perr != nil {
			t.Fatalf("ParseStreamChunk(%q,%s): %v", ev.EventType, ev.Payload, perr)
		}
		if c.UpstreamErr != nil {
			t.Fatalf("unexpected fatal upstream error: %v", c.UpstreamErr)
		}
		if !ok {
			continue // structural (messageStart / contentBlockStop)
		}
		if c.Delta != "" {
			acc = append(acc, c.Delta...)
			deltaCount++
		}
		if c.StopReason != adapter.StopUnknown {
			gotStop = c.StopReason
		}
		if c.Usage != nil {
			gotUsage = c.Usage
		}
		if c.Done {
			sawDone = true
		}
	}
	if err := <-errCh; err != nil && err != io.EOF {
		t.Fatalf("terminal stream error: %v", err)
	}

	if string(acc) != wantText {
		t.Errorf("accumulated text = %q, want %q (deltas must accumulate non-empty)", acc, wantText)
	}
	if deltaCount != len(deltas) {
		t.Errorf("delta chunks = %d, want %d", deltaCount, len(deltas))
	}
	if gotStop != adapter.StopEndTurn {
		t.Errorf("stopReason = %q, want end_turn", gotStop)
	}
	if gotUsage == nil || gotUsage.PromptTokens != 7 || gotUsage.CompletionTokens != 3 || gotUsage.TotalTokens != 10 {
		t.Errorf("metadata usage = %+v, want 7/3/10", gotUsage)
	}
	if !sawDone {
		t.Error("expected a terminal Done chunk (messageStop/metadata)")
	}
}

// ---- B3 regression (c): an exception event driven through a PUMP ------------

// stubDecryptorBR is a Decryptor that returns a fixed key (the relay package has
// no exported stub; this mirrors adapter.stubDecryptor for these white-box tests).
type stubDecryptorBR struct{}

func (stubDecryptorBR) Decrypt(*model.Channel) (string, error) { return "k", nil }

const brSecret = "0123456789abcdef0123456789abcdef"

// newBedrockStreamDB opens an isolated in-memory sqlite DB with the Channel table.
func newBedrockStreamDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:brtest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// seedBedrockChan inserts a Bedrock channel (region us-east-1) and returns its id.
func seedBedrockChan(t *testing.T, gdb *gorm.DB, models []string) uint {
	t.Helper()
	enc, err := crypto.Encrypt(brSecret, "bedrock-bearer-key")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mj, _ := json.Marshal(models)
	ch := &model.Channel{
		Name:   "br",
		Type:   model.ChannelBedrock,
		Region: "us-east-1",
		Key:    enc,
		Models: mj,
		Status: model.ChannelEnabled,
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch.ID
}

// runPumpStream drives the PRODUCTION stream.go pumpStream over a REAL TCP
// listener (gin's c.Stream requires an http.CloseNotifier writer, which the
// ResponseRecorder lacks — the existing streaming tests use the same real-server
// pattern). The mock upstream server serves the given event-stream wire bytes;
// the handler builds the upstream *http.Response from it and calls pumpStream.
// It returns the captured (usage, completion, streamErr).
func runPumpStream(t *testing.T, wire []byte) (*adapter.Usage, string, error) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(wire)
	}))
	defer upstream.Close()

	var (
		gotUsage *adapter.Usage
		gotComp  string
		gotErr   error
	)
	r := &Relayer{}
	rc := &requestContext{format: FormatOpenAI, uni: adapter.UnifiedRequest{Model: "anthropic.claude"}}
	ad := adapter.NewBedrockAdapter(stubDecryptorBR{})

	front := gin.New()
	front.POST("/run", func(c *gin.Context) {
		resp, err := http.Get(upstream.URL)
		if err != nil {
			t.Errorf("get upstream: %v", err)
			return
		}
		defer resp.Body.Close()
		setSSEHeaders(c)
		gotUsage, gotComp, _, gotErr = r.pumpStream(c, rc, ad, resp, time.Now())
	})
	frontSrv := httptest.NewServer(front)
	defer frontSrv.Close()

	resp, err := http.Post(frontSrv.URL+"/run", "application/json", nil)
	if err != nil {
		t.Fatalf("post front: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	return gotUsage, gotComp, gotErr
}

// TestPumpStream_ExceptionEvent_ProductionPumpFatal proves the B2 fix in the
// PRODUCTION /v1 pump (stream.go pumpStream), the second of the two pumps: a
// Bedrock validationException frame fed through upstreamEvents must surface as a
// FATAL stream error (the returned streamErr), NOT be swallowed as a malformed
// frame.
func TestPumpStream_ExceptionEvent_ProductionPumpFatal(t *testing.T) {
	const exMsg = "model id is invalid"
	var wire []byte
	wire = append(wire, bedrockFrame("messageStart", `{"role":"assistant"}`)...)
	jb, _ := json.Marshal(exMsg)
	wire = append(wire, bedrockFrame("validationException", `{"message":`+string(jb)+`}`)...)

	usage, completion, streamErr := runPumpStream(t, wire)
	if streamErr == nil {
		t.Fatal("production pump must return a FATAL error for an exception event (not swallow it as 200-empty)")
	}
	if !strings.Contains(streamErr.Error(), exMsg) {
		t.Errorf("streamErr = %v, want it to contain the upstream message %q", streamErr, exMsg)
	}
	// Confirm the mapped http status would be 400 for validationException.
	if got := upstreamHTTPStatus(streamErr); got != http.StatusBadRequest {
		t.Errorf("upstreamHTTPStatus = %d, want 400", got)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil (exception before any usage)", usage)
	}
	if completion != "" {
		t.Errorf("completion = %q, want empty", completion)
	}
}

// TestPumpStream_EmptyStream_Warning proves the empty-stream condition on the
// PRODUCTION /v1 path: a stream that ends cleanly with no text and no usage
// triggers the readable emptyStreamWarning in error_message, distinct from a
// genuine non-empty reply.
func TestPumpStream_EmptyStream_Warning(t *testing.T) {
	// messageStart + messageStop only — structural, no delta, no usage.
	var wire []byte
	wire = append(wire, bedrockFrame("messageStart", `{"role":"assistant"}`)...)
	wire = append(wire, bedrockFrame("messageStop", `{"stopReason":"end_turn"}`)...)

	usage, completion, streamErr := runPumpStream(t, wire)
	if streamErr != nil {
		t.Fatalf("clean empty stream must not be a fatal error: %v", streamErr)
	}
	if completion != "" || usage != nil {
		t.Fatalf("expected empty completion + nil usage, got completion=%q usage=%+v", completion, usage)
	}
	// serveStream writes emptyStreamWarning into error_message under exactly this
	// condition (completion=="" && usage==nil).
	if emptyStreamWarning == "" {
		t.Error("emptyStreamWarning must be a non-empty readable message")
	}
}

// newBedrockStreamRouter mounts the test-chat handler with a stub middleware that
// sets the admin uid context key JWTAuth normally sets (no real auth chain —
// relay cannot import middleware).
func newBedrockStreamRouter(ctrl *TestChatController, adminUID uint) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("uid", adminUID); c.Next() })
	r.POST("/api/channels/:id/test-chat", ctrl.TestChat)
	return r
}

// TestBedrockStream_ExceptionEvent_PumpSetsErrorVisible is B3 regression (c) and
// the B2 proof on the test-chat path: a mock ConverseStream emits a
// validationException frame (:event-type=validationException, body {message}).
// Driven through the real TestChat handler — and thus testchat.go's
// pumpOpenAIStream — the fatal UpstreamErr must NOT be swallowed as a malformed
// frame; the is_test log row must record status=error with the upstream message
// in error_message (instead of a clean 200-empty).
func TestBedrockStream_ExceptionEvent_PumpSetsErrorVisible(t *testing.T) {
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
		// A normal opening event, then a fatal exception mid-stream.
		write(bedrockFrame("messageStart", `{"role":"assistant"}`))
		jb, _ := json.Marshal(exMsg)
		write(bedrockFrame("validationException", `{"message":`+string(jb)+`}`))
	}))
	defer srv.Close()

	gdb := newBedrockStreamDB(t)
	// Migrate request_logs too so the is_test row persists and we can assert on it.
	if err := gdb.AutoMigrate(&model.RequestLog{}); err != nil {
		t.Fatalf("migrate request_logs: %v", err)
	}
	id := seedBedrockChan(t, gdb, []string{"anthropic.claude"})

	chSvc := service.NewChannelService(gdb, nil, brSecret)
	logSvc := service.NewLogService(gdb)
	ctrl := NewTestChatController(chSvc, logSvc, nil)
	// Redirect the (otherwise hardcoded AWS) streaming round-trip to the mock,
	// preserving the real BuildRequest body/headers.
	ctrl.streamDo = func(req *http.Request) (*http.Response, error) {
		mu, _ := http.NewRequest(req.Method, srv.URL+req.URL.Path, req.Body)
		mu.Header = req.Header.Clone()
		return http.DefaultClient.Do(mu)
	}

	r := newBedrockStreamRouter(ctrl, 9)
	front := httptest.NewServer(r)
	defer front.Close()

	body := `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/api/channels/"+itoa(id)+"/test-chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The is_test row must capture the fatal upstream error (NOT a silent success).
	var rows []model.RequestLog
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request_logs rows = %d, want 1; stream body=%s", len(rows), raw)
	}
	got := rows[0]
	if got.Status != model.LogError {
		t.Errorf("log Status = %q, want error (exception must NOT be swallowed → 200-empty)", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, exMsg) {
		t.Errorf("log ErrorMessage = %q, want it to contain the upstream exception message %q", got.ErrorMessage, exMsg)
	}
	if got.HTTPStatus != http.StatusBadRequest {
		t.Errorf("log HTTPStatus = %d, want 400 (validationException maps to 400)", got.HTTPStatus)
	}
	if !got.IsStream || !got.IsTest {
		t.Errorf("log flags: IsStream=%v IsTest=%v, want both true", got.IsStream, got.IsTest)
	}
}
