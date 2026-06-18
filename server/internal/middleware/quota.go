package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/relay"
	"github.com/agent-router/server/internal/service"
)

// Quota returns middleware that performs the pre-flight two-level quota check
// (Tech Design §4). It must run after RelayAuth. Using the authenticated token
// and user it asks the QuotaService for the combined remaining headroom
// (min(token remaining, user remaining), -1 = unlimited); when nothing remains
// (remaining <= 0 and not unlimited) it aborts with 402 in the inbound format's
// error schema. The relay handler later performs the precise estimate-vs-headroom
// admission once the request body is parsed; this middleware is the cheap guard
// that rejects already-exhausted keys before any work.
func Quota(quota *service.QuotaService) gin.HandlerFunc {
	return func(c *gin.Context) {
		format := relay.FormatFromPath(c)

		tok, ok := CurrentRelayToken(c)
		if !ok {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication, "authentication required")
			return
		}
		user, ok := CurrentRelayUser(c)
		if !ok {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication, "authentication required")
			return
		}

		remaining, err := quota.CheckRemaining(c.Request.Context(), tok.ID, user.ID, 0)
		if err != nil {
			relay.WriteClassError(c, format, 500, relay.ClassInternal, "could not check quota")
			return
		}
		// remaining < 0 means unlimited at the binding level; only a bounded,
		// non-positive remaining is an exhausted-quota rejection.
		if remaining == 0 {
			relay.WriteClassError(c, format, 402, relay.ClassQuota,
				"insufficient quota: the API key or account has no remaining token budget")
			return
		}

		c.Next()
	}
}
