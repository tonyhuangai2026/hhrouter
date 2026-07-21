package relay

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
)

// Channel-metadata exposure (Tech Design): once a relay request is served by a
// concrete upstream channel, the response advertises WHICH channel actually
// handled it via three response headers derived from the final successful
// channel (lastAtt.channel):
//   - X-Router-Channel-Id / X-Router-Channel-Name / X-Router-Channel-Type
//
// Headers only — the channel is deliberately NOT injected into the response
// body, so the body stays a faithful passthrough of the upstream response shape.
// Available on both streaming and non-streaming responses. setChannelHeaders is
// pure and treats a nil channel as a no-op, so the error paths (which have no
// successful channel) simply skip it.

const (
	headerChannelID   = "X-Router-Channel-Id"
	headerChannelName = "X-Router-Channel-Name"
	headerChannelType = "X-Router-Channel-Type"
)

// setChannelHeaders writes the X-Router-Channel-* response headers for the
// channel that served the request. It MUST be called before the response
// headers are committed (before c.JSON / WriteHeader). ch == nil is a no-op.
//
// HTTP header values should be ASCII; a channel Name may contain non-ASCII (e.g.
// Chinese) characters, so its header value is percent-encoded (RFC 3986 style):
// every byte outside the unreserved-ish printable-ASCII set is emitted as %XX.
// The Id and Type values are always ASCII and written verbatim. The raw UTF-8
// name is still exposed unescaped in the body via channelInfoMap.
func setChannelHeaders(c *gin.Context, ch *model.Channel) {
	if ch == nil {
		return
	}
	h := c.Writer.Header()
	h.Set(headerChannelID, strconv.FormatUint(uint64(ch.ID), 10))
	h.Set(headerChannelName, percentEncodeHeaderValue(ch.Name))
	h.Set(headerChannelType, string(ch.Type))
}

// percentEncodeHeaderValue renders s as an ASCII-safe HTTP header value by
// percent-encoding every byte that is not a printable ASCII character in the
// unreserved / commonly-safe set. This keeps plain ASCII names (the common case)
// byte-for-byte identical while making non-ASCII names (e.g. UTF-8 Chinese)
// transmissible in a header that should be ASCII-only.
func percentEncodeHeaderValue(s string) string {
	const upperhex = "0123456789ABCDEF"
	// Fast path: leave the string untouched when it is already all-safe.
	safe := true
	for i := 0; i < len(s); i++ {
		if !isSafeHeaderByte(s[i]) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	buf := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		b := s[i]
		if isSafeHeaderByte(b) {
			buf = append(buf, b)
			continue
		}
		buf = append(buf, '%', upperhex[b>>4], upperhex[b&0x0f])
	}
	return string(buf)
}

// isSafeHeaderByte reports whether b may appear verbatim in the percent-encoded
// header value. It permits printable ASCII except '%' (the escape marker) so the
// encoding is unambiguously reversible.
func isSafeHeaderByte(b byte) bool {
	return b >= 0x20 && b < 0x7f && b != '%'
}

// maybeInjectSystemCache sets uni.SystemCacheControl to auto-cache the system
// prompt when the chosen channel opted in (AutoCacheSystem) and the system is
// long enough to be cacheable on the target model. It NEVER overrides a
// breakpoint the client already set (uni.SystemCacheControl != nil), and only
// applies to bedrock/anthropic channels (OpenAI upstream auto-caches, so its
// channels are skipped). The min-length threshold is model-dependent: Haiku
// requires 4096 tokens, other models 1024 (per the Tech Design); a system prompt
// estimated below the threshold is left uncached because Bedrock silently skips
// caching a too-short prefix, making an injected breakpoint pointless.
//
// uni is expected to be the per-attempt value copy (mutated via pointer) so the
// injection does not leak into rc.uni or later failover candidates.
func maybeInjectSystemCache(uni *adapter.UnifiedRequest, ch *model.Channel, upstreamModel string) {
	if uni.SystemCacheControl != nil {
		return // client intent wins — never override or stack breakpoints
	}
	if !ch.AutoCacheSystem {
		return
	}
	if ch.Type != model.ChannelBedrock && ch.Type != model.ChannelAnthropic {
		return
	}
	sys := strings.TrimSpace(uni.System)
	if sys == "" {
		return
	}
	threshold := 1024
	if strings.Contains(strings.ToLower(upstreamModel), "haiku") {
		threshold = 4096
	}
	if adapter.EstimateSystemTokens(sys) < threshold {
		return
	}
	uni.SystemCacheControl = &adapter.CacheControl{Type: "ephemeral"}
}
