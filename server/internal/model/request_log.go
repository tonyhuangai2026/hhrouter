package model

import "time"

// InboundFormat enumerates the API dialect the downstream client used (derived
// from the endpoint). This is a DIFFERENT axis from OutputFormat: inbound is how
// the REQUEST was parsed (and what request_log.inbound_format records); output
// is how the RESPONSE is rendered.
type InboundFormat string

const (
	InboundOpenAI    InboundFormat = "openai"
	InboundAnthropic InboundFormat = "anthropic"
)

// OutputFormat enumerates the response rendering dialect a key may pin
// (Token.OutputFormat). Unlike InboundFormat it also includes bedrock, since a
// key can request Bedrock-shaped responses even though the platform has no
// Bedrock inbound endpoint. Empty/unset = follow the endpoint (= the inbound
// dialect).
type OutputFormat string

const (
	OutputOpenAI    OutputFormat = "openai"
	OutputAnthropic OutputFormat = "anthropic"
	OutputBedrock   OutputFormat = "bedrock"
)

// ValidOutputFormat reports whether s is an accepted output-format value for a
// token: empty (follow endpoint) or one of the three rendering dialects.
func ValidOutputFormat(s string) bool {
	switch OutputFormat(s) {
	case "", OutputOpenAI, OutputAnthropic, OutputBedrock:
		return true
	default:
		return false
	}
}

// LogStatus enumerates the outcome of a relayed request.
type LogStatus string

const (
	LogSuccess LogStatus = "success"
	LogError   LogStatus = "error"
)

// RequestLog records a single relayed request for accounting and auditing.
//
// Indexes (per Tech Design §3): (created_at), (user_id, created_at),
// (channel_id, created_at), (model).
type RequestLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `gorm:"index;index:idx_request_logs_user_created,priority:2;index:idx_request_logs_channel_created,priority:2" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	UserID uint `gorm:"not null;index:idx_request_logs_user_created,priority:1" json:"user_id"`
	// TokenID is nullable: production /v1 relay traffic is keyed by a downstream
	// API key (a non-nil id), but admin test-chat traffic (IsTest=true) is not
	// associated with any token and writes NULL. Readers/aggregations treat a nil
	// token_id as non-key traffic; no query filters on it. (Tech Design §3.1)
	TokenID   *uint `json:"token_id"`
	ChannelID uint  `gorm:"not null;index:idx_request_logs_channel_created,priority:1" json:"channel_id"`
	RuleID    *uint `json:"rule_id,omitempty"` // nullable

	Model         string `gorm:"type:varchar(128);index" json:"model"`    // external model name
	UpstreamModel string `gorm:"type:varchar(128)" json:"upstream_model"` // actual upstream model id

	InboundFormat InboundFormat `gorm:"type:varchar(16);not null" json:"inbound_format"`

	PromptTokens     int `gorm:"not null;default:0" json:"prompt_tokens"`
	CompletionTokens int `gorm:"not null;default:0" json:"completion_tokens"`
	TotalTokens      int `gorm:"not null;default:0" json:"total_tokens"`

	// CacheReadTokens / CacheWriteTokens are prompt-cache token counts captured
	// from the upstream usage (priced on their own tiers). CostMicroUSD is the
	// computed cost of this request in micro-USD (1 USD = 1_000_000), from the
	// (channel, model) price × usage. All three are nullable: legacy rows and any
	// row where cost could not be computed (e.g. an error before pricing) stay
	// NULL. AutoMigrate adds these nullable columns; no migrate-time ALTER needed.
	CacheReadTokens  *int   `gorm:"default:null" json:"cache_read_tokens"`
	CacheWriteTokens *int   `gorm:"default:null" json:"cache_write_tokens"`
	CostMicroUSD     *int64 `gorm:"default:null" json:"cost_micro_usd"`

	Status       LogStatus `gorm:"type:varchar(16);not null" json:"status"`
	HTTPStatus   int       `gorm:"not null;default:0" json:"http_status"`
	ErrorMessage string    `gorm:"type:text" json:"error_message,omitempty"` // nullable

	LatencyMs int  `gorm:"not null;default:0" json:"latency_ms"`
	// FirstTokenMs is the time-to-first-token (TTFT) in milliseconds for a
	// streaming request: the wall-clock from the start of the upstream stream to
	// the first non-empty content delta. It is nullable and stream-only — nil for
	// non-streaming requests, for streams that errored before any delta, and for
	// legacy rows written before this column existed. Total latency (LatencyMs) is
	// meaningless for a stream (it tracks until the connection closes), so the UI
	// shows TTFT for stream rows instead. AutoMigrate adds this nullable column;
	// no migrate-time ALTER is needed.
	FirstTokenMs *int `gorm:"default:null" json:"first_token_ms"`
	IsStream     bool `gorm:"not null;default:false" json:"is_stream"`

	// IsTest marks a row written by the admin direct test-chat path (Tech Design
	// §3): it consumes no quota and is not keyed by a token. The dashboard
	// summary/timeseries default to production-only (IsTest=false) so test
	// traffic never pollutes production metrics; the logs listing can opt in.
	IsTest bool `gorm:"not null;default:false;index" json:"is_test"`
}
