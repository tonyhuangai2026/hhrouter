package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
)

// ErrPriceNotConfigured reports that a price row does not exist. Delete returns
// it for an unknown id. Lookup does NOT: an unpriced model is served for free
// (see Lookup).
var ErrPriceNotConfigured = errors.New("model price not configured")

// microUSDPerMillionDivisor is the denominator that turns
// (tokens × microUSDPerMillion) into micro-USD: prices are quoted per 1,000,000
// tokens, so cost = tokens × price / 1_000_000.
const microUSDPerMillionDivisor int64 = 1_000_000

// PricingService owns the per-(channel, model) price table and the pure cost
// computation. It is the single source of truth for "is this model billable"
// (Lookup) and "what does this usage cost" (Cost). Prices are micro-USD per 1M
// tokens (see model.ModelPrice); costs are micro-USD.
type PricingService struct {
	db *gorm.DB
}

// NewPricingService constructs a PricingService.
func NewPricingService(db *gorm.DB) *PricingService {
	return &PricingService{db: db}
}

// Lookup returns the price row for (channelID, model), or (nil, nil) when the
// model has no usable price — no row at all, or a row whose input/output price is
// not strictly positive.
//
// An unpriced model is treated as FREE rather than as an error: refusing to serve
// a model purely because nobody has filled in its price turns a bookkeeping gap
// into an outage, and operators kept hitting it after adding a model to a channel.
// Cost(nil, …) is 0, so such a request is served, logged with cost 0, and debits
// nothing.
//
// The tradeoff is deliberate and worth knowing: because quotas are enforced in
// USD, a model left unpriced is effectively unmetered — it consumes no key or user
// balance and contributes nothing to billing. Configure prices for anything whose
// spend must be capped.
//
// Only a genuine database failure returns a non-nil error.
func (s *PricingService) Lookup(channelID uint, modelName string) (*model.ModelPrice, error) {
	var p model.ModelPrice
	err := s.db.Where("channel_id = ? AND model = ?", channelID, modelName).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.InputMicroUSDPerM <= 0 || p.OutputMicroUSDPerM <= 0 {
		// A row with a non-positive price is as unusable as no row, and treating
		// the two differently would only make misconfiguration harder to diagnose.
		return nil, nil
	}
	return &p, nil
}

// Cost computes the micro-USD cost of a request's token usage under a price row.
// It is a pure function: cost = ceilDiv(
//
//	prompt*input + completion*output + cacheRead*cacheRead + cacheWrite*cacheWrite,
//	1_000_000)
//
// where each price is micro-USD per 1M tokens. Missing cache tokens or missing
// cache prices contribute 0 (their products are 0). A nil price yields 0 (the
// relay only computes cost on the success path, which always has a price; error
// paths pass nil so the logged cost stays NULL). Rounding is half-up-to-the-next
// micro-USD (ceilDiv) so a non-zero usage never rounds down to a free request.
func (s *PricingService) Cost(price *model.ModelPrice, u adapter.Usage) int64 {
	if price == nil {
		return 0
	}
	numerator := int64(u.PromptTokens)*price.InputMicroUSDPerM +
		int64(u.CompletionTokens)*price.OutputMicroUSDPerM +
		int64(u.CacheReadTokens)*price.CacheReadMicroUSDPerM +
		int64(u.CacheWriteTokens)*price.CacheWriteMicroUSDPerM
	if numerator <= 0 {
		return 0
	}
	// ceilDiv(numerator, 1_000_000): round up so a tiny-but-nonzero cost is at
	// least 1 micro-USD rather than silently free.
	return (numerator + microUSDPerMillionDivisor - 1) / microUSDPerMillionDivisor
}

// ListByChannel returns all price rows for a channel (empty slice if none).
// Used by the admin pricing page to populate per-model price inputs.
func (s *PricingService) ListByChannel(channelID uint) ([]model.ModelPrice, error) {
	var rows []model.ModelPrice
	if err := s.db.Where("channel_id = ?", channelID).Order("model ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// List returns every price row (admin, all channels).
func (s *PricingService) List() ([]model.ModelPrice, error) {
	var rows []model.ModelPrice
	if err := s.db.Order("channel_id ASC, model ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Upsert creates or updates the price row for (channelID, model). All four
// prices are micro-USD per 1M tokens; the caller is responsible for validating
// non-negativity and the input/output>0 requirement (the controller does this).
// It keys on the (channel_id, model) unique index.
func (s *PricingService) Upsert(channelID uint, modelName string, input, output, cacheRead, cacheWrite int64) (*model.ModelPrice, error) {
	var p model.ModelPrice
	err := s.db.Where("channel_id = ? AND model = ?", channelID, modelName).First(&p).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		p = model.ModelPrice{
			ChannelID:              channelID,
			Model:                  modelName,
			InputMicroUSDPerM:      input,
			OutputMicroUSDPerM:     output,
			CacheReadMicroUSDPerM:  cacheRead,
			CacheWriteMicroUSDPerM: cacheWrite,
		}
		if err := s.db.Create(&p).Error; err != nil {
			return nil, err
		}
		return &p, nil
	case err != nil:
		return nil, err
	}
	p.InputMicroUSDPerM = input
	p.OutputMicroUSDPerM = output
	p.CacheReadMicroUSDPerM = cacheRead
	p.CacheWriteMicroUSDPerM = cacheWrite
	if err := s.db.Save(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// Delete removes a price row by id. Returns ErrPriceNotConfigured if no row
// matched (so the controller can render a 404).
func (s *PricingService) Delete(id uint) error {
	res := s.db.Delete(&model.ModelPrice{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPriceNotConfigured
	}
	return nil
}
