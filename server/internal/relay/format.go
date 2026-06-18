// Package relay is the public LLM gateway (Tech Design §4/§6/§8). It wires the
// routing engine (T5), the upstream adapters (T6), the two-level quota service
// (T4) and the request-log service (T8) into the inbound endpoints exposed under
// /v1: OpenAI Chat Completions, Anthropic Messages and a model listing.
//
// The relay owns the http.Client round-trip, SSE streaming, failover across
// candidate channels, usage accounting and request logging. The adapter package
// stays a pure library; this package is the only place that performs the actual
// upstream call.
//
// Files:
//   - format.go : inbound-format detection + §11 error schema rendering.
//   - usage.go  : maps adapter.Usage onto the request_log token fields.
//   - relay.go  : non-streaming relay (parse → select → call → failover → adapt).
//   - stream.go : SSE streaming relay (incl. Bedrock event-stream de-framing).
package relay

import (
	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/model"
)

// InboundFormat identifies which inbound wire dialect a request used. It mirrors
// model.InboundFormat but is kept local so the relay can attach format-specific
// error rendering behaviour to it.
type InboundFormat = model.InboundFormat

const (
	// FormatOpenAI is the OpenAI Chat Completions dialect (POST /v1/chat/completions).
	FormatOpenAI = model.InboundOpenAI
	// FormatAnthropic is the Anthropic Messages dialect (POST /v1/messages).
	FormatAnthropic = model.InboundAnthropic
)

// OutputFormat identifies the dialect a RESPONSE is rendered in. Unlike
// InboundFormat (how the request was parsed) it also includes bedrock — a key
// may pin Bedrock-shaped responses even though there is no Bedrock inbound
// endpoint. See model.OutputFormat.
type OutputFormat = model.OutputFormat

const (
	OutOpenAI    = model.OutputOpenAI
	OutAnthropic = model.OutputAnthropic
	OutBedrock   = model.OutputBedrock
)

// outFormatFor resolves the response output format from the inbound dialect and
// an optional per-key override. An empty/nil override → mirror the inbound
// dialect (today's behavior). An unrecognized override defensively falls back to
// the inbound dialect too (token values are validated at create/update time).
func outFormatFor(inbound InboundFormat, override *string) OutputFormat {
	if override != nil {
		switch OutputFormat(*override) {
		case OutOpenAI, OutAnthropic, OutBedrock:
			return OutputFormat(*override)
		}
	}
	// Mirror the inbound dialect (openai/anthropic) as the default output.
	return OutputFormat(string(inbound))
}

// ErrorBodyOut builds an error payload in the OUTPUT format's error schema.
// OpenAI: {error:{message,type}}; Anthropic: {type:"error",error:{type,message}};
// Bedrock Converse: {message:...} (the Converse error body shape).
func ErrorBodyOut(out OutputFormat, errType, message string) any {
	switch out {
	case OutBedrock:
		return gin.H{"message": message}
	case OutAnthropic:
		return gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}}
	default:
		return gin.H{"error": gin.H{"message": message, "type": errType}}
	}
}

// errTypeOut returns the output-format-appropriate error "type" string. Bedrock
// carries no type in its {message} body, so it is unused there; openai/anthropic
// reuse the inbound vocabulary via ErrType.
func errTypeOut(out OutputFormat, class ErrorClass) string {
	if out == OutAnthropic {
		return ErrType(FormatAnthropic, class)
	}
	return ErrType(FormatOpenAI, class)
}

// WriteOutError renders an error in the OUTPUT format's schema and aborts. Used
// by the relay handle/serve path once a key's output format is resolved; the
// pre-handle/middleware path keeps using WriteClassError (endpoint dialect).
func WriteOutError(c *gin.Context, out OutputFormat, status int, class ErrorClass, message string) {
	c.AbortWithStatusJSON(status, ErrorBodyOut(out, errTypeOut(out, class), message))
}

// WriteError renders an error in the inbound format's error schema (Tech Design
// §11) and aborts the request. OpenAI uses {error:{message,type}}; Anthropic uses
// {type:"error",error:{type,message}}. errType is the format-appropriate error
// type string (e.g. "invalid_request_error", "authentication_error").
func WriteError(c *gin.Context, format InboundFormat, status int, errType, message string) {
	c.AbortWithStatusJSON(status, ErrorBody(format, errType, message))
}

// ErrorBody builds (but does not write) the inbound-format error payload. It is
// exported so middleware can render errors before a handler runs and so tests can
// assert on the exact shape.
func ErrorBody(format InboundFormat, errType, message string) any {
	if format == FormatAnthropic {
		return gin.H{
			"type": "error",
			"error": gin.H{
				"type":    errType,
				"message": message,
			},
		}
	}
	// Default to the OpenAI schema.
	return gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	}
}

// ErrType returns the format-appropriate "type" string for a broad error class.
// Anthropic and OpenAI use slightly different vocabularies for the same classes,
// so this normalises a small set the relay needs.
func ErrType(format InboundFormat, class ErrorClass) string {
	if format == FormatAnthropic {
		switch class {
		case ClassInvalidRequest:
			return "invalid_request_error"
		case ClassAuthentication:
			return "authentication_error"
		case ClassPermission:
			return "permission_error"
		case ClassQuota:
			return "rate_limit_error"
		case ClassUpstream:
			return "api_error"
		default:
			return "api_error"
		}
	}
	switch class {
	case ClassInvalidRequest:
		return "invalid_request_error"
	case ClassAuthentication:
		return "authentication_error"
	case ClassPermission:
		return "permission_error"
	case ClassQuota:
		return "insufficient_quota"
	case ClassUpstream:
		return "upstream_error"
	default:
		return "api_error"
	}
}

// ErrorClass is a provider-agnostic error category mapped to a format-specific
// type string by ErrType.
type ErrorClass int

const (
	// ClassInternal is an unexpected server-side failure (500).
	ClassInternal ErrorClass = iota
	// ClassInvalidRequest is a malformed/invalid inbound request (400).
	ClassInvalidRequest
	// ClassAuthentication is a missing/invalid/expired key (401).
	ClassAuthentication
	// ClassPermission is an authenticated-but-forbidden action (403).
	ClassPermission
	// ClassQuota is quota exhaustion (402).
	ClassQuota
	// ClassUpstream is a failure talking to (all) upstream channel(s) (502).
	ClassUpstream
)

// WriteClassError is a convenience wrapper around WriteError that derives the
// type string from an ErrorClass for the given inbound format.
func WriteClassError(c *gin.Context, format InboundFormat, status int, class ErrorClass, message string) {
	WriteError(c, format, status, ErrType(format, class), message)
}

// FormatFromPath infers the inbound format from the request path so middleware
// (which runs before the handler binds a body) can render errors in the right
// schema. /v1/messages → Anthropic; everything else under the relay → OpenAI.
func FormatFromPath(c *gin.Context) InboundFormat {
	if c.FullPath() == "/v1/messages" {
		return FormatAnthropic
	}
	return FormatOpenAI
}
