package relay

import (
	"bytes"
	"testing"

	"github.com/agent-router/server/internal/adapter"
)

// TestBedrockEncoderRoundTripsThroughDecoder proves the OUTBOUND encoder
// (adapter.EncodeBedrockFrame) produces frames that the PRODUCTION inbound
// de-framer (readEventStream + eventTypeFromHeaders) decodes back to the exact
// (eventType, payload). This is the cross-compatibility guarantee: a downstream
// Bedrock client (which uses the same AWS framing) will deframe our output the
// same way the relay deframes upstream Bedrock — prelude/header/CRC correctness
// is proven by the real decoder, not a hand-rolled check.
func TestBedrockEncoderRoundTripsThroughDecoder(t *testing.T) {
	type frame struct {
		eventType string
		payload   string
	}
	frames := []frame{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hello"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":", world"}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`},
	}

	// Encode every frame back-to-back into one stream, exactly as the relay would
	// write them to the client.
	var wire bytes.Buffer
	for _, f := range frames {
		wire.Write(adapter.EncodeBedrockFrame(f.eventType, []byte(f.payload)))
	}

	// Decode via the production de-framer.
	out := make(chan streamEvent, 16)
	errc := make(chan error, 1)
	go func() { errc <- readEventStream(bytes.NewReader(wire.Bytes()), out); close(out) }()

	var got []streamEvent
	for ev := range out {
		got = append(got, streamEvent{EventType: ev.EventType, Payload: append([]byte(nil), ev.Payload...)})
	}
	if err := <-errc; err != nil {
		t.Fatalf("readEventStream: %v", err)
	}

	if len(got) != len(frames) {
		t.Fatalf("decoded %d events, want %d", len(got), len(frames))
	}
	for i, f := range frames {
		if got[i].EventType != f.eventType {
			t.Errorf("event[%d] type = %q, want %q", i, got[i].EventType, f.eventType)
		}
		if string(got[i].Payload) != f.payload {
			t.Errorf("event[%d] payload = %q, want %q", i, got[i].Payload, f.payload)
		}
	}
}
