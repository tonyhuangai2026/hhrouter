package adapter

import "fmt"

// UpstreamError represents a non-2xx response (or an unparseable body) from an
// upstream provider. The relay (T7) inspects StatusCode to decide failover
// (5xx/429 → try next candidate) and maps the error back to the inbound format's
// error schema (Tech Design §11). Body holds a trimmed snippet of the upstream
// response for diagnostics; it is never logged in full alongside credentials.
type UpstreamError struct {
	// Provider is the adapter name ("openai" / "bedrock").
	Provider string
	// StatusCode is the upstream HTTP status (0 if the failure was not an HTTP
	// status, e.g. a malformed success body).
	StatusCode int
	// Message is a human-readable summary.
	Message string
	// Body is a trimmed snippet of the raw upstream body (may be empty).
	Body string
}

func (e *UpstreamError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s upstream error (status %d): %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s upstream error: %s", e.Provider, e.Message)
}

// Retryable reports whether the relay should fail over to another candidate
// channel for this error. 5xx and 429 (throttling) are retryable; 4xx client
// errors are not (re-issuing won't help).
func (e *UpstreamError) Retryable() bool {
	return e.StatusCode == 429 || (e.StatusCode >= 500 && e.StatusCode <= 599)
}

// snippet trims a raw body for inclusion in an error/diagnostic message.
func snippet(b []byte) string {
	const max = 500
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
