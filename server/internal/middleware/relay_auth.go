package middleware

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/relay"
	"github.com/agent-router/server/internal/service"
)

// Gin context keys populated by RelayAuth for the relay handlers.
const (
	// CtxRelayToken holds the authenticated *model.Token.
	CtxRelayToken = "relay_token"
	// CtxRelayUser holds the owning *model.User.
	CtxRelayUser = "relay_user"
)

// RelayAuth returns middleware that authenticates a downstream relay request by
// its API key (Tech Design §4). The key is taken from `Authorization: Bearer
// sk-...` (OpenAI style) or the Anthropic `x-api-key` header. The token is looked
// up by sha256 key_hash, its status/expiry validated, and the owning user loaded
// and required to be enabled. On success the token and user are injected into the
// Gin context for the quota middleware and relay handlers. Failures abort with
// 401 in the inbound format's error schema.
func RelayAuth(tokens *service.TokenService, db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		format := relay.FormatFromPath(c)

		key, ok := relayKey(c)
		if !ok {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication,
				"missing API key: provide an Authorization: Bearer sk-... or x-api-key header")
			return
		}

		tok, err := tokens.GetByKeyHash(service.HashKey(key))
		if err != nil {
			if errors.Is(err, service.ErrTokenNotFound) {
				relay.WriteClassError(c, format, 401, relay.ClassAuthentication, "invalid API key")
				return
			}
			relay.WriteClassError(c, format, 500, relay.ClassInternal, "could not validate API key")
			return
		}

		if msg, ok := tokenUsable(tok); !ok {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication, msg)
			return
		}

		var user model.User
		if err := db.First(&user, tok.UserID).Error; err != nil {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication, "owning account not found")
			return
		}
		if user.Status != model.UserEnabled {
			relay.WriteClassError(c, format, 401, relay.ClassAuthentication, "owning account is disabled")
			return
		}

		c.Set(CtxRelayToken, tok)
		c.Set(CtxRelayUser, &user)
		c.Next()
	}
}

// tokenUsable reports whether a token may be used right now, returning a reason
// when it may not. Expiry is enforced even if the status column still reads
// "enabled" (the expiry timestamp is authoritative).
func tokenUsable(tok *model.Token) (string, bool) {
	switch tok.Status {
	case model.TokenDisabled:
		return "API key is disabled", false
	case model.TokenExpired:
		return "API key has expired", false
	}
	if tok.ExpiredAt != nil && !tok.ExpiredAt.IsZero() && time.Now().After(*tok.ExpiredAt) {
		return "API key has expired", false
	}
	return "", true
}

// relayKey extracts the downstream API key from the request: it prefers the
// Anthropic `x-api-key` header, then falls back to `Authorization: Bearer ...`.
func relayKey(c *gin.Context) (string, bool) {
	if v := strings.TrimSpace(c.GetHeader("x-api-key")); v != "" {
		return v, true
	}
	h := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		if tok := strings.TrimSpace(h[len(prefix):]); tok != "" {
			return tok, true
		}
	}
	return "", false
}

// CurrentRelayToken returns the authenticated token from the context.
func CurrentRelayToken(c *gin.Context) (*model.Token, bool) {
	v, ok := c.Get(CtxRelayToken)
	if !ok {
		return nil, false
	}
	tok, ok := v.(*model.Token)
	return tok, ok
}

// CurrentRelayUser returns the owning user from the context.
func CurrentRelayUser(c *gin.Context) (*model.User, bool) {
	v, ok := c.Get(CtxRelayUser)
	if !ok {
		return nil, false
	}
	u, ok := v.(*model.User)
	return u, ok
}
