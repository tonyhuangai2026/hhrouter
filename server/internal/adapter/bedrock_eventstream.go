package adapter

import (
	"encoding/binary"
	"hash/crc32"
)

// bedrock_eventstream.go implements the OUTBOUND encoder for the AWS
// vnd.amazon.eventstream wire format used by Bedrock ConverseStream, so the
// relay can render a streamed response as real Bedrock binary frames when a key
// pins output_format=bedrock. This is the inverse of the relay's inbound
// de-framer (readEventStream/eventTypeFromHeaders): a frame produced here
// round-trips back through that decoder identically (proven by test).
//
// Frame layout (all multi-byte integers big-endian):
//
//	[ prelude: totalLen(4) | headersLen(4) | preludeCRC(4) ]
//	[ headers... ][ payload... ][ messageCRC(4) ]
//
// preludeCRC is crc32(IEEE) over the first 8 prelude bytes; messageCRC is
// crc32(IEEE) over everything from the start of the frame through the payload
// (i.e. prelude + preludeCRC + headers + payload). Each header is encoded as:
//
//	nameLen(1) | name | valueType(1) | valueLen(2) | value
//
// We only emit string-typed (valueType 7) headers, which is all Bedrock uses
// for the discriminator headers below.

// bedrockHeaderString is the AWS event-stream header value-type for a UTF-8
// string (the only type this encoder emits).
const bedrockHeaderString = 7

// EncodeBedrockFrame builds one AWS event-stream frame carrying the given
// :event-type and (already-serialized JSON) payload. It stamps the standard
// discriminator headers Bedrock ConverseStream sends — :event-type, the JSON
// :content-type, and :message-type=event — in a fixed order, and computes both
// CRCs. The returned bytes are a complete, self-delimiting frame ready to write
// to the client.
func EncodeBedrockFrame(eventType string, payload []byte) []byte {
	headers := encodeBedrockEventHeaders(eventType)

	totalLen := 4 + 4 + 4 + len(headers) + len(payload) + 4
	buf := make([]byte, 0, totalLen)

	// Prelude: totalLen + headersLen (8 bytes), then its CRC.
	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	buf = append(buf, prelude[:]...)

	var preludeCRC [4]byte
	binary.BigEndian.PutUint32(preludeCRC[:], crc32.ChecksumIEEE(prelude[:]))
	buf = append(buf, preludeCRC[:]...)

	// Headers + payload.
	buf = append(buf, headers...)
	buf = append(buf, payload...)

	// Message CRC covers everything written so far (prelude+preludeCRC+headers+payload).
	var msgCRC [4]byte
	binary.BigEndian.PutUint32(msgCRC[:], crc32.ChecksumIEEE(buf))
	buf = append(buf, msgCRC[:]...)
	return buf
}

// encodeBedrockEventHeaders encodes the three standard Bedrock event headers in
// a fixed, deterministic order. Header decode is order-independent, but a fixed
// order keeps frames reproducible.
func encodeBedrockEventHeaders(eventType string) []byte {
	var out []byte
	out = appendBedrockStringHeader(out, ":event-type", eventType)
	out = appendBedrockStringHeader(out, ":content-type", "application/json")
	out = appendBedrockStringHeader(out, ":message-type", "event")
	return out
}

// appendBedrockStringHeader appends one string-typed event-stream header.
func appendBedrockStringHeader(out []byte, name, value string) []byte {
	out = append(out, byte(len(name)))
	out = append(out, name...)
	out = append(out, bedrockHeaderString)
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(value)))
	out = append(out, vl[:]...)
	out = append(out, value...)
	return out
}
