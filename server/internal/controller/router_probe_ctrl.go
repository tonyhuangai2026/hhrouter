package controller

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router"
	"github.com/agent-router/server/internal/router/probe"
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

// testRequest is the POST /api/router-probe/test body: an optional URL to test
// (falls back to the saved one when empty), so the admin can test what's typed
// before saving.
type testRequest struct {
	URL string `json:"url"`
}

// testResult is the connectivity-test response.
type testResult struct {
	OK        bool              `json:"ok"`
	LatencyMs int64             `json:"latency_ms"`
	Error     string            `json:"error,omitempty"`
	Result    *probe.Prediction `json:"result,omitempty"`
}

// Test handles POST /api/router-probe/test: send ONE real classification request
// to the given (or saved) proxy URL with a demo conversation prompt and report
// whether it succeeded, the round-trip latency, and the parsed {w,t} prediction.
// This never touches the mock — it always exercises the real HTTP path so the
// admin can confirm the proxy is reachable and returns the expected shape.
func (c *RouterProbeController) Test(ctx *gin.Context) {
	var in testRequest
	_ = ctx.ShouldBindJSON(&in) // body is optional
	url := strings.TrimSpace(in.URL)
	if url == "" {
		url = strings.TrimSpace(model.GetOption(c.db, model.OptRouterProbeURL, ""))
	}
	if url == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "no url to test (enter a proxy URL first)", "type": "invalid_request"}})
		return
	}

	// A short, representative conversation rendered to the classifier's prompt.
	demo := router.RenderProbePrompt("", []struct{ Role, Text string }{
		{Role: "user", Text: "please write a function to reverse a string"},
	})

	// Bound the test so a hung proxy can't hang the request handler.
	reqCtx, cancel := context.WithTimeout(ctx.Request.Context(), 8*time.Second)
	defer cancel()

	p := probe.NewHTTPProbe(url)
	start := time.Now()
	pred, err := p.Predict(reqCtx, demo)
	latency := time.Since(start).Milliseconds()

	res := testResult{OK: err == nil, LatencyMs: latency}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Result = &pred
	}
	// Always 200: the test itself succeeded even when the probe failed; the OK
	// flag carries the connectivity result for the UI.
	ctx.JSON(http.StatusOK, res)
}
