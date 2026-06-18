package model

import "time"

// ModelPrice is the per-(channel, model) price configuration that drives USD
// billing. A request is only permitted when a ModelPrice row exists for the
// resolved (channel_id, upstream model) with input & output prices > 0; the
// relay computes each request's cost from its token usage and these prices.
//
// All prices are stored as micro-USD PER 1,000,000 TOKENS (int64): e.g. a model
// that costs $3.00 per 1M input tokens has InputMicroUSDPerM = 3_000_000.
// Integer micro-USD avoids floating-point drift when costs are accumulated.
// Cost of n tokens at price p (micro-USD/1M) = n * p / 1_000_000.
//
// The cache tiers (CacheRead / CacheWrite) are optional — they default to 0 and
// only apply when the upstream reports prompt-cache token counts. Channels that
// never return cache tokens are billed purely on input/output.
type ModelPrice struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// (ChannelID, Model) is unique: at most one price row per channel+model.
	ChannelID uint   `gorm:"not null;uniqueIndex:idx_model_prices_channel_model,priority:1" json:"channel_id"`
	Model     string `gorm:"type:varchar(128);not null;uniqueIndex:idx_model_prices_channel_model,priority:2" json:"model"`

	// Prices in micro-USD per 1,000,000 tokens. Input & Output are required (>0)
	// for the model to be requestable; the two cache tiers are optional (0 = the
	// cache tokens, if any, are not charged).
	InputMicroUSDPerM      int64 `gorm:"not null;default:0" json:"input_micro_usd_per_m"`
	OutputMicroUSDPerM     int64 `gorm:"not null;default:0" json:"output_micro_usd_per_m"`
	CacheReadMicroUSDPerM  int64 `gorm:"not null;default:0" json:"cache_read_micro_usd_per_m"`
	CacheWriteMicroUSDPerM int64 `gorm:"not null;default:0" json:"cache_write_micro_usd_per_m"`
}
