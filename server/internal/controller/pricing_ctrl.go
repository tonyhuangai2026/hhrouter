package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/service"
)

// PricingController handles the admin model-pricing endpoints (Tech Design §5):
// list per-channel prices, upsert a (channel, model) price, and delete one. All
// routes are mounted behind JWTAuth()+AdminOnly() in api/router.go. Prices are
// exchanged as micro-USD per 1M tokens (int64) — the frontend converts to/from
// USD, keeping a single rounding source in the form layer.
type PricingController struct {
	pricing *service.PricingService
}

// NewPricingController constructs a PricingController.
func NewPricingController(pricing *service.PricingService) *PricingController {
	return &PricingController{pricing: pricing}
}

// pricingRequest is the JSON body for PUT /api/pricing (upsert). All prices are
// micro-USD per 1M tokens. ChannelID + Model identify the row.
type pricingRequest struct {
	ChannelID              uint   `json:"channel_id"`
	Model                  string `json:"model"`
	InputMicroUSDPerM      int64  `json:"input_micro_usd_per_m"`
	OutputMicroUSDPerM     int64  `json:"output_micro_usd_per_m"`
	CacheReadMicroUSDPerM  int64  `json:"cache_read_micro_usd_per_m"`
	CacheWriteMicroUSDPerM int64  `json:"cache_write_micro_usd_per_m"`
}

// List handles GET /api/pricing. With ?channel_id=N it returns that channel's
// price rows; without it, all rows.
func (pc *PricingController) List(c *gin.Context) {
	if raw, ok := c.GetQuery("channel_id"); ok && raw != "" {
		cid, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "invalid channel_id")
			return
		}
		rows, err := pc.pricing.ListByChannel(uint(cid))
		if err != nil {
			respondError(c, http.StatusInternalServerError, "internal_error", "could not list prices")
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
		return
	}
	rows, err := pc.pricing.List()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list prices")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// Upsert handles PUT /api/pricing. It validates that all prices are non-negative
// and that input & output are strictly positive (a model is only requestable
// with both), then creates or updates the (channel_id, model) row.
func (pc *PricingController) Upsert(c *gin.Context) {
	var req pricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if req.ChannelID == 0 || req.Model == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "channel_id and model are required")
		return
	}
	if req.InputMicroUSDPerM < 0 || req.OutputMicroUSDPerM < 0 ||
		req.CacheReadMicroUSDPerM < 0 || req.CacheWriteMicroUSDPerM < 0 {
		respondError(c, http.StatusBadRequest, "invalid_request", "prices must not be negative")
		return
	}
	if req.InputMicroUSDPerM == 0 || req.OutputMicroUSDPerM == 0 {
		respondError(c, http.StatusBadRequest, "invalid_request", "input and output prices are required (must be > 0)")
		return
	}
	row, err := pc.pricing.Upsert(req.ChannelID, req.Model,
		req.InputMicroUSDPerM, req.OutputMicroUSDPerM, req.CacheReadMicroUSDPerM, req.CacheWriteMicroUSDPerM)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not save price")
		return
	}
	c.JSON(http.StatusOK, row)
}

// Delete handles DELETE /api/pricing/:id.
func (pc *PricingController) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid price id")
		return
	}
	if err := pc.pricing.Delete(uint(id)); err != nil {
		if errors.Is(err, service.ErrPriceNotConfigured) {
			respondError(c, http.StatusNotFound, "not_found", "price not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "internal_error", "could not delete price")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
