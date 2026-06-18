package controller

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// TokenController handles the downstream API-key (sk-...) management endpoints
// (Tech Design §3 tokens, §8). All routes are mounted behind JWTAuth() in
// api/router.go and are scoped to the authenticated user: a user may only
// CRUD their own tokens (cross-user access yields 404).
type TokenController struct {
	tokens *service.TokenService
}

// NewTokenController constructs a TokenController.
func NewTokenController(tokens *service.TokenService) *TokenController {
	return &TokenController{tokens: tokens}
}

// tokenRequest is the JSON body for create/update. Pointer fields let the
// service distinguish "absent" from "set to zero value" so updates stay partial.
type tokenRequest struct {
	Name          *string    `json:"name"`
	Status        *string    `json:"status"`
	Quota         *int64     `json:"quota"`
	ExpiredAt     *time.Time `json:"expired_at"`
	Group         *string    `json:"group"`
	AllowedModels *[]string  `json:"allowed_models"`
	// OutputFormat pins the response rendering format ("" = follow endpoint,
	// else openai|anthropic|bedrock). Validated in Create/Update.
	OutputFormat *string `json:"output_format"`
}

// toInput converts the request into the service-layer input.
func (r *tokenRequest) toInput() service.TokenInput {
	in := service.TokenInput{
		Name:          r.Name,
		Quota:         r.Quota,
		ExpiredAt:     r.ExpiredAt,
		Group:         r.Group,
		AllowedModels: r.AllowedModels,
		OutputFormat:  r.OutputFormat,
	}
	if r.Status != nil {
		st := model.TokenStatus(*r.Status)
		in.Status = &st
	}
	return in
}

// validOutputFormat reports whether a tokenRequest's output_format is acceptable
// (absent, empty, or one of the three rendering dialects).
func (r *tokenRequest) validOutputFormat() bool {
	if r.OutputFormat == nil {
		return true
	}
	return model.ValidOutputFormat(*r.OutputFormat)
}

// List handles GET /api/tokens — the caller's own tokens (masked).
//
// This route is mounted behind JWTAuth ONLY (not AdminOnly), so self-service
// token management keeps working for every user. The admin "view another user's
// tokens" dimension is therefore gated INSIDE the handler: only an admin may
// pass ?user_id=<id> to list a specific user's tokens; for any non-admin the
// param is ignored and the listing is hard-scoped to their own uid (mirrors the
// dashboard ?user_id pattern in dashboard_ctrl.go).
func (tc *TokenController) List(c *gin.Context) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	target := uid
	if role, _ := middleware.CurrentUserRole(c); role == model.RoleAdmin {
		if v := c.Query("user_id"); v != "" {
			if id, err := strconv.ParseUint(v, 10, 64); err == nil {
				target = uint(id)
			}
		}
	}

	views, err := tc.tokens.List(target)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list tokens")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views, "total": len(views)})
}

// Get handles GET /api/tokens/:id (own token only).
func (tc *TokenController) Get(c *gin.Context) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	id, ok := parseTokenID(c)
	if !ok {
		return
	}
	view, err := tc.tokens.Get(uid, id)
	if err != nil {
		tc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// Create handles POST /api/tokens. The full plaintext sk- key is returned in
// the response exactly once; subsequent reads only ever show the mask.
func (tc *TokenController) Create(c *gin.Context) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if !req.validOutputFormat() {
		respondError(c, http.StatusBadRequest, "invalid_request", "output_format must be one of: openai, anthropic, bedrock (or empty to follow the endpoint)")
		return
	}
	res, err := tc.tokens.Create(uid, req.toInput())
	if err != nil {
		tc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, res)
}

// Update handles PUT /api/tokens/:id (own token only).
func (tc *TokenController) Update(c *gin.Context) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	id, ok := parseTokenID(c)
	if !ok {
		return
	}
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if !req.validOutputFormat() {
		respondError(c, http.StatusBadRequest, "invalid_request", "output_format must be one of: openai, anthropic, bedrock (or empty to follow the endpoint)")
		return
	}
	view, err := tc.tokens.Update(uid, id, req.toInput())
	if err != nil {
		tc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// Delete handles DELETE /api/tokens/:id (own token only).
func (tc *TokenController) Delete(c *gin.Context) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	id, ok := parseTokenID(c)
	if !ok {
		return
	}
	if err := tc.tokens.Delete(uid, id); err != nil {
		tc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// respondServiceError maps token service errors onto HTTP statuses. A token
// owned by another user is surfaced as 404 (ownership gating) so callers cannot
// probe for the existence of other users' tokens.
func (tc *TokenController) respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrTokenNotFound):
		respondError(c, http.StatusNotFound, "not_found", "token not found")
	case errors.Is(err, service.ErrTokenForbidden):
		respondError(c, http.StatusForbidden, "forbidden", "token does not belong to you")
	case errors.Is(err, service.ErrInvalidToken):
		respondError(c, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		respondError(c, http.StatusInternalServerError, "internal_error", "could not process request")
	}
}

// parseTokenID extracts and validates the :id path parameter.
func parseTokenID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid token id")
		return 0, false
	}
	return uint(id), true
}
