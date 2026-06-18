package service

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// LogService writes request_logs and answers the dashboard/log queries
// (Tech Design §3 request_logs, §8 dashboard/logs). All aggregation is pushed
// into SQL (GROUP BY / aggregate functions) so no full-table scan is ever
// materialised in Go memory.
//
// Time-bucketing in Timeseries is dialect-aware: PostgreSQL (production) uses
// date_trunc, SQLite (unit tests) uses strftime. Both emit a UTC bucket label
// that is normalised to RFC3339 before being returned, so the response contract
// is identical regardless of backend.
type LogService struct {
	db *gorm.DB
}

// NewLogService constructs a LogService.
func NewLogService(db *gorm.DB) *LogService {
	return &LogService{db: db}
}

// Write inserts a single request_log row. The relay (T7) calls this after every
// relayed request to record accounting/audit data. It is exported precisely so
// the relay package can depend on it.
func (s *LogService) Write(log *model.RequestLog) error {
	return s.db.Create(log).Error
}

// LogFilter carries the optional query dimensions shared by the log listing and
// the dashboard aggregations. A zero value means "no constraint" on that
// dimension. UserID, when non-nil, scopes results to a single user; the
// controller sets it to the caller's id for non-admins and to an optional
// ?user_id for admins.
type LogFilter struct {
	UserID    *uint
	ChannelID *uint
	Model     string
	Status    model.LogStatus
	Start     *time.Time
	End       *time.Time
	// IsTest scopes by the test-chat marker: nil = no constraint (all rows),
	// *false = production only, *true = test only. The dashboard controller
	// defaults summary/timeseries to *false and leaves the logs listing nil
	// (all) unless an explicit is_test param is supplied. (Tech Design §3.3)
	IsTest *bool
}

// applyFilter appends the active filter dimensions onto a query as WHERE
// clauses. It is the single place that translates a LogFilter into SQL so the
// listing and every aggregation share identical scoping (and identical
// user-isolation) semantics.
func (s *LogService) applyFilter(q *gorm.DB, f LogFilter) *gorm.DB {
	if f.UserID != nil {
		q = q.Where("user_id = ?", *f.UserID)
	}
	if f.ChannelID != nil {
		q = q.Where("channel_id = ?", *f.ChannelID)
	}
	if f.Model != "" {
		q = q.Where("model = ?", f.Model)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Start != nil {
		q = q.Where("created_at >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("created_at <= ?", *f.End)
	}
	if f.IsTest != nil {
		q = q.Where("is_test = ?", *f.IsTest)
	}
	return q
}

// LogRow is one detail row returned by List. It embeds the stored model and
// adds the joined username and channel name so the frontend table can display
// human-readable values without N+1 lookups.
type LogRow struct {
	model.RequestLog
	Username    string `json:"username"`
	ChannelName string `json:"channel_name"`
	// TokenName is the name of the downstream API key (token) that made the
	// request, joined from tokens.name. Empty for test-chat rows (token_id NULL)
	// and for rows whose token was since deleted.
	TokenName string `json:"token_name"`
	// ChannelType is the upstream kind (openai/bedrock/anthropic) joined from the
	// channel, so the listing can show which upstream actually served the request
	// (more useful than inbound_format, which for test-chat is always "openai").
	// Empty when the channel was since deleted.
	ChannelType string `json:"channel_type"`
}

// List returns a paginated, filtered slice of request_logs (newest first) and
// the total count matching the filter (for pagination). page is 1-based.
func (s *LogService) List(f LogFilter, page, pageSize int) ([]LogRow, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	base := s.applyFilter(s.db.Model(&model.RequestLog{}), f)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	rows := make([]LogRow, 0, pageSize)
	// Left joins so a log whose channel/user was since deleted still lists.
	q := s.applyFilterPrefixed(
		s.db.Table("request_logs AS rl").
			Select("rl.*, u.username AS username, c.name AS channel_name, c.type AS channel_type, t.name AS token_name").
			Joins("LEFT JOIN users u ON u.id = rl.user_id").
			Joins("LEFT JOIN channels c ON c.id = rl.channel_id").
			Joins("LEFT JOIN tokens t ON t.id = rl.token_id"),
		f,
	)
	if err := q.
		Order("rl.created_at DESC, rl.id DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// applyFilterPrefixed is the alias-aware variant of applyFilter for the joined
// listing query.
func (s *LogService) applyFilterPrefixed(q *gorm.DB, f LogFilter) *gorm.DB {
	if f.UserID != nil {
		q = q.Where("rl.user_id = ?", *f.UserID)
	}
	if f.ChannelID != nil {
		q = q.Where("rl.channel_id = ?", *f.ChannelID)
	}
	if f.Model != "" {
		q = q.Where("rl.model = ?", f.Model)
	}
	if f.Status != "" {
		q = q.Where("rl.status = ?", f.Status)
	}
	if f.Start != nil {
		q = q.Where("rl.created_at >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("rl.created_at <= ?", *f.End)
	}
	if f.IsTest != nil {
		q = q.Where("rl.is_test = ?", *f.IsTest)
	}
	return q
}

// Summary holds the aggregate dashboard headline metrics. SuccessRate is a
// fraction in [0,1]; the frontend handles both fractions and percentages.
type Summary struct {
	TotalRequests    int64   `json:"total_requests"`
	SuccessRequests  int64   `json:"success_requests"`
	ErrorRequests    int64   `json:"error_requests"`
	SuccessRate      float64 `json:"success_rate"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	// CostMicroUSD is the total billed cost over the range in micro-USD
	// (1 USD = 1_000_000). NULL costs (legacy/un-priced rows) COALESCE to 0.
	CostMicroUSD int64 `json:"cost_micro_usd"`
}

// Summary computes the headline metrics for the dashboard in a single grouped
// aggregate query (no per-row load). All sums COALESCE to 0 so an empty range
// yields a well-formed zeroed Summary rather than NULLs.
func (s *LogService) Summary(f LogFilter) (*Summary, error) {
	var raw struct {
		TotalRequests    int64
		SuccessRequests  int64
		PromptTokens     int64
		CompletionTokens int64
		TotalTokens      int64
		AvgLatencyMs     float64
		CostMicroUSD     int64
	}

	q := s.applyFilter(s.db.Model(&model.RequestLog{}), f).
		Select(
			"COUNT(*) AS total_requests, " +
				"COALESCE(SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0) AS success_requests, " +
				"COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, " +
				"COALESCE(SUM(completion_tokens), 0) AS completion_tokens, " +
				"COALESCE(SUM(total_tokens), 0) AS total_tokens, " +
				"COALESCE(AVG(latency_ms), 0) AS avg_latency_ms, " +
				"COALESCE(SUM(cost_micro_usd), 0) AS cost_micro_usd",
		)
	if err := q.Scan(&raw).Error; err != nil {
		return nil, err
	}

	out := &Summary{
		TotalRequests:    raw.TotalRequests,
		SuccessRequests:  raw.SuccessRequests,
		ErrorRequests:    raw.TotalRequests - raw.SuccessRequests,
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		TotalTokens:      raw.TotalTokens,
		AvgLatencyMs:     raw.AvgLatencyMs,
		CostMicroUSD:     raw.CostMicroUSD,
	}
	if raw.TotalRequests > 0 {
		out.SuccessRate = float64(raw.SuccessRequests) / float64(raw.TotalRequests)
	}
	return out, nil
}

// TimePoint is one bucket within a timeseries group.
type TimePoint struct {
	TS       string `json:"ts"` // RFC3339 UTC bucket start
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// TimeseriesGroup is the per-dimension series the frontend renders. Group is the
// dimension value ("all" when ungrouped); Points are ordered by bucket ascending.
type TimeseriesGroup struct {
	Group  string      `json:"group"`
	Points []TimePoint `json:"points"`
}

// Timeseries returns request-count and token-usage buckets over time, optionally
// split by a dimension. interval is "hour" or "day" (anything else falls back to
// "day"); groupBy is "channel", "model" or empty (single "all" series).
//
// The bucketing and grouping are done entirely in SQL via GROUP BY, and the
// rows are stitched into per-group series in a single pass.
func (s *LogService) Timeseries(f LogFilter, interval, groupBy string) ([]TimeseriesGroup, error) {
	bucketExpr, err := s.bucketExpr(interval)
	if err != nil {
		return nil, err
	}

	groupCol, useGroup := groupColumn(groupBy)

	selectCols := bucketExpr + " AS bucket, " +
		"COUNT(*) AS requests, COALESCE(SUM(total_tokens), 0) AS tokens"
	if useGroup {
		selectCols = groupCol + " AS grp, " + selectCols
	}

	q := s.applyFilter(s.db.Model(&model.RequestLog{}), f).Select(selectCols)
	if useGroup {
		q = q.Group("grp, bucket").Order("grp ASC, bucket ASC")
	} else {
		q = q.Group("bucket").Order("bucket ASC")
	}

	var rows []struct {
		Grp      string
		Bucket   string
		Requests int64
		Tokens   int64
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}

	// Stitch ordered rows into per-group series preserving SQL ordering.
	order := make([]string, 0)
	byGroup := make(map[string]*TimeseriesGroup)
	for _, r := range rows {
		key := "all"
		if useGroup {
			key = r.Grp
			if key == "" {
				key = "unknown"
			}
		}
		g, ok := byGroup[key]
		if !ok {
			g = &TimeseriesGroup{Group: key, Points: []TimePoint{}}
			byGroup[key] = g
			order = append(order, key)
		}
		g.Points = append(g.Points, TimePoint{
			TS:       normalizeBucket(r.Bucket),
			Requests: r.Requests,
			Tokens:   r.Tokens,
		})
	}

	out := make([]TimeseriesGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byGroup[k])
	}
	return out, nil
}

// groupColumn maps the API group_by value to a safe column name. Only the two
// whitelisted dimensions are accepted, so the value can never be attacker-
// controlled SQL.
func groupColumn(groupBy string) (string, bool) {
	switch groupBy {
	case "channel", "channel_id":
		return "channel_id", true
	case "model":
		return "model", true
	default:
		return "", false
	}
}

// bucketExpr returns the dialect-specific SQL expression that truncates
// created_at to the requested interval. Only "hour" and "day" are honoured;
// any other value defaults to "day".
func (s *LogService) bucketExpr(interval string) (string, error) {
	unit := "day"
	if interval == "hour" {
		unit = "hour"
	}

	switch s.db.Dialector.Name() {
	case "postgres":
		// date_trunc yields a timestamptz; cast to text for a stable label.
		return fmt.Sprintf("to_char(date_trunc('%s', created_at), 'YYYY-MM-DD\"T\"HH24:MI:SS')", unit), nil
	case "sqlite":
		if unit == "hour" {
			return "strftime('%Y-%m-%dT%H:00:00', created_at)", nil
		}
		return "strftime('%Y-%m-%dT00:00:00', created_at)", nil
	default:
		return "", fmt.Errorf("log: unsupported dialect %q for timeseries", s.db.Dialector.Name())
	}
}

// normalizeBucket turns a dialect bucket label into an RFC3339 UTC timestamp so
// the frontend's new Date(ts) parses consistently regardless of backend.
func normalizeBucket(b string) string {
	if b == "" {
		return b
	}
	// Both dialects emit "2006-01-02T15:04:05"; parse as UTC and re-emit RFC3339.
	if t, err := time.Parse("2006-01-02T15:04:05", b); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	// Postgres may include a timezone/space form; try a couple of fallbacks.
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, b); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return b
}
