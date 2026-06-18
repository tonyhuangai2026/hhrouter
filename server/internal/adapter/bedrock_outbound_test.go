package adapter

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// TestBuildBedrockResponse covers the Converse non-stream response shape, the
// stop-reason mapping, usage, and the empty-content guard.
func TestBuildBedrockResponse(t *testing.T) {
	r := UnifiedResponse{
		Model:      "anthropic.claude",
		Content:    []ContentBlock{TextBlock("hello "), TextBlock("world")},
		StopReason: StopEndTurn,
		Usage:      Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14},
	}
	body := BuildBedrockResponse(r)

	output, ok := body["output"].(map[string]any)
	if !ok {
		t.Fatalf("missing output: %+v", body)
	}
	msg := output["message"].(map[string]any)
	if msg["role"] != RoleAssistant {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	content := msg["content"].([]map[string]any)
	if len(content) != 1 || content[0]["text"] != "hello \nworld" {
		t.Errorf("content = %+v, want single block 'hello \\nworld'", content)
	}
	if body["stopReason"] != stopToBedrock(StopEndTurn) {
		t.Errorf("stopReason = %v, want %v", body["stopReason"], stopToBedrock(StopEndTurn))
	}
	usage := body["usage"].(map[string]any)
	if usage["inputTokens"] != 11 || usage["outputTokens"] != 3 || usage["totalTokens"] != 14 {
		t.Errorf("usage = %+v, want 11/3/14", usage)
	}

	// Empty content → one {text:""} block.
	empty := BuildBedrockResponse(UnifiedResponse{StopReason: StopEndTurn})
	ec := empty["output"].(map[string]any)["message"].(map[string]any)["content"].([]map[string]any)
	if len(ec) != 1 || ec[0]["text"] != "" {
		t.Errorf("empty content = %+v, want [{text:\"\"}]", ec)
	}
}

// TestBuildBedrockStreamEvent covers the contentBlockDelta mapping and the
// lifecycle-event payload helpers.
func TestBuildBedrockStreamEvent(t *testing.T) {
	et, p, ok := BuildBedrockStreamEvent(StreamChunk{Delta: "Hi"})
	if !ok || et != BedrockEventContentBlockDelta {
		t.Fatalf("delta event = %q ok=%v", et, ok)
	}
	if p["contentBlockIndex"] != 0 || p["delta"].(map[string]any)["text"] != "Hi" {
		t.Errorf("delta payload = %+v", p)
	}
	// No text → not ok (stop/usage emitted as explicit lifecycle events).
	if _, _, ok := BuildBedrockStreamEvent(StreamChunk{StopReason: StopEndTurn}); ok {
		t.Error("non-delta chunk should be ok=false")
	}

	if BedrockMessageStartPayload()["role"] != RoleAssistant {
		t.Error("messageStart role")
	}
	if BedrockContentBlockStopPayload()["contentBlockIndex"] != 0 {
		t.Error("contentBlockStop index")
	}
	if BedrockMessageStopPayload(StopMaxTokens)["stopReason"] != stopToBedrock(StopMaxTokens) {
		t.Error("messageStop stopReason")
	}
	mu := BedrockMetadataPayload(Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7})["usage"].(map[string]any)
	if mu["inputTokens"] != 5 || mu["outputTokens"] != 2 || mu["totalTokens"] != 7 {
		t.Errorf("metadata usage = %+v", mu)
	}
}

// TestEncodeBedrockFrame_Structure self-checks the binary layout (prelude
// lengths + both CRCs) without depending on the relay decoder. The relay-package
// round-trip test proves cross-compatibility with the production de-framer.
func TestEncodeBedrockFrame_Structure(t *testing.T) {
	payload := []byte(`{"contentBlockIndex":0,"delta":{"text":"Hi"}}`)
	frame := EncodeBedrockFrame("contentBlockDelta", payload)

	if len(frame) < 16 {
		t.Fatalf("frame too short: %d", len(frame))
	}
	totalLen := binary.BigEndian.Uint32(frame[0:4])
	headersLen := binary.BigEndian.Uint32(frame[4:8])
	if int(totalLen) != len(frame) {
		t.Errorf("totalLen = %d, want %d", totalLen, len(frame))
	}
	// preludeCRC over first 8 bytes.
	if got := binary.BigEndian.Uint32(frame[8:12]); got != crc32.ChecksumIEEE(frame[0:8]) {
		t.Errorf("prelude CRC mismatch: %d", got)
	}
	// messageCRC (last 4 bytes) over everything before it.
	msgCRC := binary.BigEndian.Uint32(frame[len(frame)-4:])
	if got := crc32.ChecksumIEEE(frame[:len(frame)-4]); got != msgCRC {
		t.Errorf("message CRC mismatch: got %d want %d", got, msgCRC)
	}
	// headers + payload + 4(CRC) must fit after the 12-byte prelude+preludeCRC.
	if 12+int(headersLen)+len(payload)+4 != len(frame) {
		t.Errorf("section sizes inconsistent: headers=%d payload=%d total=%d", headersLen, len(payload), len(frame))
	}
}
