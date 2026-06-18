package controller

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// DashboardController serves the request-log analytics endpoints (Tech Design
// §8): GET /api/dashboard/summary, GET /api/dashboard/timeseries and
// GET /api/logs. All three are mounted behind JWTAuth(): a normal user sees
// only their own rows (the filter is hard-scoped to their uid), while an admin
// sees everything and may additionally narrow to a single ?user_id.
type DashboardController struct {
	logs *service.LogService
}

// NewDashboardController constructs a DashboardController.
func NewDashboardController(logs *service.LogService) *DashboardController {
	return &DashboardController{logs: logs}
}

// buildFilter assembles a LogFilter from the query string, enforcing
// user-isolation: a non-admin caller is always pinned to their own uid and any
// ?user_id they pass is ignored. An admin caller is unscoped by default and may
// opt into a single user via ?user_id. Returns false (after writing an error)
// if the request could not be authenticated.
//
// isTestDefault sets the is_test scope used when the request carries no explicit
// ?is_test param: the logs listing passes nil (all rows) to preserve the prior
// behaviour, while summary/timeseries pass *false so production metrics are not
// polluted by test-chat traffic. An explicit ?is_test (true|false) always wins;
// ?is_test=all (or any other value) clears the constraint to "all rows".
func (dc *DashboardController) buildFilter(c *gin.Context, isTestDefault *bool) (service.LogFilter, bool) {
	uid, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return service.LogFilter{}, false
	}
	role, _ := middleware.CurrentUserRole(c)

	var f service.LogFilter

	if role == model.RoleAdmin {
		// Admin: optional explicit user filter, otherwise full visibility.
		if v := c.Query("user_id"); v != "" {
			if id, err := strconv.ParseUint(v, 10, 64); err == nil {
				u := uint(id)
				f.UserID = &u
			}
		}
	} else {
		// Non-admin: hard-scope to the caller; ignore any user_id they sent.
		u := uid
		f.UserID = &u
	}

	if v := c.Query("channel_id"); v != "" {
		if id, err := strconv.ParseUint(v, 10, 64); err == nil {
			ch := uint(id)
			f.ChannelID = &ch
		}
	}
	if v := c.Query("model"); v != "" {
		f.Model = v
	}
	if v := c.Query("status"); v == string(model.LogSuccess) || v == string(model.LogError) {
		f.Status = model.LogStatus(v)
	}
	if t, ok := parseTime(c.Query("start")); ok {
		f.Start = &t
	}
	if t, ok := parseTime(c.Query("end")); ok {
		f.End = &t
	}
	f.IsTest = resolveIsTest(c.Query("is_test"), isTestDefault)
	return f, true
}

// resolveIsTest maps the optional ?is_test query value onto a LogFilter.IsTest:
//   - "true"/"1"           → *true  (test traffic only)
//   - "false"/"0"          → *false (production traffic only)
//   - absent ("")          → def    (endpoint default)
//   - any other (e.g. all) → nil    (no constraint, all rows)
func resolveIsTest(v string, def *bool) *bool {
	switch v {
	case "":
		return def
	case "true", "1":
		b := true
		return &b
	case "false", "0":
		b := false
		return &b
	default:
		return nil
	}
}

// prodOnly is the default is_test scope for the aggregation endpoints: exclude
// test-chat traffic so production dashboards are not skewed by it.
var prodOnly = func() *bool { b := false; return &b }()

// Summary handles GET /api/dashboard/summary. Defaults to production-only
// traffic (is_test=false); pass ?is_test=true or ?is_test=all to include test.
func (dc *DashboardController) Summary(c *gin.Context) {
	f, ok := dc.buildFilter(c, prodOnly)
	if !ok {
		return
	}
	sum, err := dc.logs.Summary(f)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not compute summary")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sum})
}

// Timeseries handles GET /api/dashboard/timeseries. Query params: interval
// (hour|day, default day) and group_by (channel|model, default none). Defaults
// to production-only traffic (is_test=false); pass ?is_test to include test.
func (dc *DashboardController) Timeseries(c *gin.Context) {
	f, ok := dc.buildFilter(c, prodOnly)
	if !ok {
		return
	}
	interval := c.DefaultQuery("interval", "day")
	groupBy := c.Query("group_by")

	series, err := dc.logs.Timeseries(f, interval, groupBy)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not compute timeseries")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":     series,
		"interval": normalizeInterval(interval),
		"group_by": groupBy,
	})
}

// Logs handles GET /api/logs — paginated detail rows with the same filters.
// Unlike the aggregation endpoints it defaults to ALL traffic (nil is_test) to
// preserve the prior behaviour; pass ?is_test=true (test only) or false
// (production only) to scope the listing.
func (dc *DashboardController) Logs(c *gin.Context) {
	f, ok := dc.buildFilter(c, nil)
	if !ok {
		return
	}
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	rows, total, err := dc.logs.List(f, page, pageSize)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list logs")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":      rows,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// normalizeInterval echoes back the effective bucket size.
func normalizeInterval(interval string) string {
	if interval == "hour" {
		return "hour"
	}
	return "day"
}

// parseTime accepts an RFC3339 timestamp (what the SPA sends via toISOString)
// and reports whether it parsed. A blank or malformed value yields (zero,false)
// so the dimension is simply left unconstrained.
func parseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	// Tolerate a date-only form.
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseIntDefault parses a positive int, falling back to def on any error.
func parseIntDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
