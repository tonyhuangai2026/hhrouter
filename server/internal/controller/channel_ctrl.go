package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// ChannelController handles the admin channel-management endpoints
// (Tech Design §8): CRUD plus model auto-fetch and connectivity test. All
// routes are mounted behind JWTAuth()+AdminOnly() in api/router.go.
type ChannelController struct {
	channels *service.ChannelService
}

// NewChannelController constructs a ChannelController.
func NewChannelController(channels *service.ChannelService) *ChannelController {
	return &ChannelController{channels: channels}
}

// channelRequest is the JSON body for create/update. Pointer fields let the
// service distinguish "absent" from "set to zero value" so updates stay partial
// and the key is only re-encrypted when explicitly supplied.
type channelRequest struct {
	Name         *string            `json:"name"`
	Type         *string            `json:"type"`
	BaseURL      *string            `json:"base_url"`
	Key          *string            `json:"key"`
	Region       *string            `json:"region"`
	Models       *[]string          `json:"models"`
	ModelMapping *map[string]string `json:"model_mapping"`
	Group        *string            `json:"group"`
	Priority     *int               `json:"priority"`
	Weight       *int               `json:"weight"`
	Status       *string            `json:"status"`
	// UseInferenceProfile is a pointer to distinguish "unset" (create → DB
	// default true) from an explicit false (update).
	UseInferenceProfile *bool `json:"use_inference_profile"`
}

// toInput converts the request into the service-layer input.
func (r *channelRequest) toInput() service.ChannelInput {
	in := service.ChannelInput{
		Name:         r.Name,
		BaseURL:      r.BaseURL,
		Key:          r.Key,
		Region:       r.Region,
		Models:       r.Models,
		ModelMapping: r.ModelMapping,
		Group:        r.Group,
		Priority:     r.Priority,
		Weight:       r.Weight,

		UseInferenceProfile: r.UseInferenceProfile,
	}
	if r.Type != nil {
		t := model.ChannelType(*r.Type)
		in.Type = &t
	}
	if r.Status != nil {
		st := model.ChannelStatus(*r.Status)
		in.Status = &st
	}
	return in
}

// List handles GET /api/channels.
func (cc *ChannelController) List(c *gin.Context) {
	views, err := cc.channels.List()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list channels")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views, "total": len(views)})
}

// Get handles GET /api/channels/:id.
func (cc *ChannelController) Get(c *gin.Context) {
	id, ok := parseChannelID(c)
	if !ok {
		return
	}
	view, err := cc.channels.Get(id)
	if err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// Create handles POST /api/channels.
func (cc *ChannelController) Create(c *gin.Context) {
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	view, err := cc.channels.Create(req.toInput())
	if err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, view)
}

// Update handles PUT /api/channels/:id.
func (cc *ChannelController) Update(c *gin.Context) {
	id, ok := parseChannelID(c)
	if !ok {
		return
	}
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	view, err := cc.channels.Update(id, req.toInput())
	if err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// Delete handles DELETE /api/channels/:id.
func (cc *ChannelController) Delete(c *gin.Context) {
	id, ok := parseChannelID(c)
	if !ok {
		return
	}
	if err := cc.channels.Delete(id); err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// FetchModels handles POST /api/channels/:id/fetch-models. An optional
// ?refresh=true query skips the Redis cache and forces a live re-fetch.
func (cc *ChannelController) FetchModels(c *gin.Context) {
	id, ok := parseChannelID(c)
	if !ok {
		return
	}
	refresh := parseBoolQuery(c, "refresh")
	res, err := cc.channels.FetchModels(c.Request.Context(), id, refresh)
	if err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// Test handles POST /api/channels/:id/test.
func (cc *ChannelController) Test(c *gin.Context) {
	id, ok := parseChannelID(c)
	if !ok {
		return
	}
	res, err := cc.channels.TestChannel(c.Request.Context(), id)
	if err != nil {
		cc.respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// respondServiceError maps service errors onto HTTP statuses.
func (cc *ChannelController) respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrChannelNotFound):
		respondError(c, http.StatusNotFound, "not_found", "channel not found")
	case errors.Is(err, service.ErrInvalidChannel):
		respondError(c, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		respondError(c, http.StatusInternalServerError, "internal_error", "could not process request")
	}
}

// parseBoolQuery reports whether the named query parameter is set to a truthy
// value ("true", "1", "yes", or present with an empty value e.g. ?refresh).
func parseBoolQuery(c *gin.Context, name string) bool {
	v, ok := c.GetQuery(name)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes":
		return true
	default:
		return false
	}
}

// parseChannelID extracts and validates the :id path parameter.
func parseChannelID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid channel id")
		return 0, false
	}
	return uint(id), true
}
