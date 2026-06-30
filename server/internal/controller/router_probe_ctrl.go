package controller

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// RouterProbeController exposes the routing-classifier ("small model" probe)
// settings: whether to use the built-in deterministic mock or call a real HTTP
// proxy in front of the SageMaker endpoint, plus that proxy URL and region.
type RouterProbeController struct {
	db *gorm.DB
}

// NewRouterProbeController constructs the controller.
func NewRouterProbeController(db *gorm.DB) *RouterProbeController {
	return &RouterProbeController{db: db}
}

// probeSettings is the GET/PUT payload shape.
type probeSettings struct {
	// Mock: when true, route expressions referencing w/t use the deterministic
	// mock classifier and NO external request is made. When false, the engine
	// POSTs to URL (if set) for real predictions.
	Mock   bool   `json:"mock"`
	URL    string `json:"url"`
	Region string `json:"region"`
}

// Get handles GET /api/router-probe.
func (c *RouterProbeController) Get(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, probeSettings{
		// Default mock=true (the safe default when unset).
		Mock:   model.GetOption(c.db, model.OptRouterProbeMock, "true") != "false",
		URL:    model.GetOption(c.db, model.OptRouterProbeURL, ""),
		Region: model.GetOption(c.db, model.OptRouterProbeRegion, ""),
	})
}

// Put handles PUT /api/router-probe.
func (c *RouterProbeController) Put(ctx *gin.Context) {
	var in probeSettings
	if err := ctx.ShouldBindJSON(&in); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid body", "type": "invalid_request"}})
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	in.Region = strings.TrimSpace(in.Region)

	// When switching to real (mock=false), require a usable http(s) URL so the
	// admin can't silently disable routing-probe predictions.
	if !in.Mock {
		if in.URL == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "url is required when mock is off", "type": "invalid_request"}})
			return
		}
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "url must be a valid http(s) URL", "type": "invalid_request"}})
			return
		}
	}

	mockVal := "true"
	if !in.Mock {
		mockVal = "false"
	}
	for _, kv := range []struct{ k, v string }{
		{model.OptRouterProbeMock, mockVal},
		{model.OptRouterProbeURL, in.URL},
		{model.OptRouterProbeRegion, in.Region},
	} {
		if err := model.SetOption(c.db, kv.k, kv.v); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to save settings", "type": "internal"}})
			return
		}
	}
	ctx.JSON(http.StatusOK, in)
}
