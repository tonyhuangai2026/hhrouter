package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// RuleController handles the admin routing-rule endpoints (Tech Design §8):
// CRUD over routing_rules. All routes are mounted behind JWTAuth()+AdminOnly()
// in api/router.go.
type RuleController struct {
	rules  *service.RuleService
	tokens *service.TokenService
}

// NewRuleController constructs a RuleController. tokens is used only to populate
// the rule editor's group dropdown (distinct token/channel groups).
func NewRuleController(rules *service.RuleService, tokens *service.TokenService) *RuleController {
	return &RuleController{rules: rules, tokens: tokens}
}

// Groups handles GET /api/rule-groups: the distinct routing groups in use, for
// the rule editor's "key groups" dropdown.
func (c *RuleController) Groups(ctx *gin.Context) {
	groups, err := c.tokens.DistinctGroups()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to list groups", "type": "internal"}})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"groups": groups})
}

// ruleRequest is the JSON body for create/update. Pointer fields let the
// service distinguish "absent" from "set to zero value" so updates stay partial.
type ruleRequest struct {
	Name             *string          `json:"name"`
	Enabled          *bool            `json:"enabled"`
	Priority         *int             `json:"priority"`
	Match            *model.MatchSpec `json:"match"`
	TargetChannelIDs *[]uint          `json:"target_channel_ids"`
	TargetGroup      *string          `json:"target_group"`
	Expr             *string          `json:"expr"`
}

// toInput converts the request into the service-layer input.
func (r *ruleRequest) toInput() service.RuleInput {
	return service.RuleInput{
		Name:             r.Name,
		Enabled:          r.Enabled,
		Priority:         r.Priority,
		Match:            r.Match,
		TargetChannelIDs: r.TargetChannelIDs,
		TargetGroup:      r.TargetGroup,
		Expr:             r.Expr,
	}
}

// List handles GET /api/rules.
func (rc *RuleController) List(c *gin.Context) {
	rules, err := rc.rules.List()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list rules")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rules, "total": len(rules)})
}

// Get handles GET /api/rules/:id.
func (rc *RuleController) Get(c *gin.Context) {
	id, ok := parseRuleID(c)
	if !ok {
		return
	}
	rule, err := rc.rules.Get(id)
	if err != nil {
		rc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, rule)
}

// Create handles POST /api/rules.
func (rc *RuleController) Create(c *gin.Context) {
	var req ruleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	rule, err := rc.rules.Create(req.toInput())
	if err != nil {
		rc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, rule)
}

// Update handles PUT /api/rules/:id.
func (rc *RuleController) Update(c *gin.Context) {
	id, ok := parseRuleID(c)
	if !ok {
		return
	}
	var req ruleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	rule, err := rc.rules.Update(id, req.toInput())
	if err != nil {
		rc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, rule)
}

// Delete handles DELETE /api/rules/:id.
func (rc *RuleController) Delete(c *gin.Context) {
	id, ok := parseRuleID(c)
	if !ok {
		return
	}
	if err := rc.rules.Delete(id); err != nil {
		rc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// respondServiceError maps rule service errors onto HTTP statuses.
func (rc *RuleController) respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrRuleNotFound):
		respondError(c, http.StatusNotFound, "not_found", "routing rule not found")
	case errors.Is(err, service.ErrInvalidRule):
		respondError(c, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		respondError(c, http.StatusInternalServerError, "internal_error", "could not process request")
	}
}

// parseRuleID extracts and validates the :id path parameter.
func parseRuleID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid rule id")
		return 0, false
	}
	return uint(id), true
}
