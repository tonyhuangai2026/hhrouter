package middleware

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/agent-router/server/internal/model"
)

// Gin context keys populated by JWTAuth for downstream handlers.
const (
	// CtxUserID holds the authenticated user's ID (uint).
	CtxUserID = "uid"
	// CtxUserRole holds the authenticated user's role (model.UserRole).
	CtxUserRole = "role"
)

// tokenTTL is how long an issued admin JWT remains valid.
const tokenTTL = 24 * time.Hour

// Claims are the custom HS256 JWT claims for the admin console (Tech Design §4):
// uid, role, exp.
type Claims struct {
	UID  uint           `json:"uid"`
	Role model.UserRole `json:"role"`
	jwt.RegisteredClaims
}

// IssueToken signs an HS256 JWT for the given user using the provided secret.
// The token carries uid/role claims and expires after tokenTTL.
func IssueToken(secret string, uid uint, role model.UserRole) (string, error) {
	now := time.Now()
	claims := Claims{
		UID:  uid,
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// ParseToken verifies an HS256 JWT signature and expiry, returning its claims.
func ParseToken(secret, tokenString string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// JWTAuth returns middleware that requires a valid HS256 bearer token. On
// success it stores uid/role in the Gin context; otherwise it aborts with 401.
func JWTAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString, ok := bearerToken(c)
		if !ok {
			abortUnauthorized(c, "missing or malformed Authorization header")
			return
		}
		claims, err := ParseToken(secret, tokenString)
		if err != nil {
			abortUnauthorized(c, "invalid or expired token")
			return
		}
		c.Set(CtxUserID, claims.UID)
		c.Set(CtxUserRole, claims.Role)
		c.Next()
	}
}

// AdminOnly returns middleware that requires the authenticated user to have the
// admin role. It must run after JWTAuth. Non-admins receive 403.
func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := c.Get(CtxUserRole)
		if !ok {
			abortUnauthorized(c, "authentication required")
			return
		}
		if role != model.RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"message": "admin privileges required",
					"type":    "forbidden",
				},
			})
			return
		}
		c.Next()
	}
}

// CurrentUserID returns the authenticated user's ID from the context.
func CurrentUserID(c *gin.Context) (uint, bool) {
	v, ok := c.Get(CtxUserID)
	if !ok {
		return 0, false
	}
	id, ok := v.(uint)
	return id, ok
}

// CurrentUserRole returns the authenticated user's role from the context.
func CurrentUserRole(c *gin.Context) (model.UserRole, bool) {
	v, ok := c.Get(CtxUserRole)
	if !ok {
		return "", false
	}
	role, ok := v.(model.UserRole)
	return role, ok
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(c *gin.Context) (string, bool) {
	h := c.GetHeader("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"message": msg,
			"type":    "unauthorized",
		},
	})
}
