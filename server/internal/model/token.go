package model

import (
	"time"

	"gorm.io/datatypes"
)

// TokenStatus enumerates the state of a downstream API key.
type TokenStatus string

const (
	TokenEnabled  TokenStatus = "enabled"
	TokenDisabled TokenStatus = "disabled"
	TokenExpired  TokenStatus = "expired"
)

// QuotaUnlimited is the sentinel quota value meaning "no limit".
const QuotaUnlimited int64 = -1

// Token is a downstream API key (sk-...) used by relay clients.
type Token struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	UserID uint `gorm:"index;not null" json:"user_id"`
	Name   string `gorm:"type:varchar(128);not null" json:"name"`

	// Key holds the plaintext key beginning with "sk-" only transiently: it is
	// shown to the user once on creation and is NOT persisted (the plaintext is
	// never stored — only its sha256 KeyHash is). It is a nullable pointer so the
	// unique index permits many rows with no stored plaintext (multiple SQL NULLs
	// are distinct under a unique index). Lookups always go through KeyHash.
	Key *string `gorm:"type:varchar(128);uniqueIndex" json:"key,omitempty"`
	// KeyHash is the sha256 of the plaintext key, used as the lookup index column.
	KeyHash string `gorm:"type:varchar(64);uniqueIndex;not null" json:"-"`

	Status TokenStatus `gorm:"type:varchar(16);not null;default:'enabled'" json:"status"`

	// Quota / UsedQuota are in micro-USD (1 USD = 1_000_000); -1 = unlimited
	// (QuotaUnlimited). USD billing debits UsedQuota by each request's computed
	// micro-USD cost. (Unit changed from tokens; values are not auto-converted.)
	Quota     int64 `gorm:"not null;default:-1" json:"quota"` // -1 = unlimited, else micro-USD
	UsedQuota int64 `gorm:"not null;default:0" json:"used_quota"` // consumed, micro-USD

	ExpiredAt *time.Time `json:"expired_at,omitempty"` // nullable

	// Group is the routing group this key belongs to.
	Group string `gorm:"type:varchar(64);not null;default:'default'" json:"group"`

	// AllowedModels optionally restricts the usable models (JSONB string array,
	// nullable; empty = no restriction).
	AllowedModels datatypes.JSON `gorm:"type:jsonb" json:"allowed_models,omitempty"`

	// OutputFormat optionally pins the RESPONSE rendering format for this key
	// (openai | anthropic | bedrock), overriding the endpoint-derived dialect so a
	// downstream system gets the format it expects regardless of which inbound
	// endpoint it calls. Nullable; nil/empty = follow the endpoint (current
	// behavior). The request body is still parsed by the endpoint — only the
	// response (body, stream frames, error schema) is rendered in this format.
	// See model.OutputFormat constants. The request log's inbound_format keeps
	// recording the TRUE inbound dialect and is NOT affected by this field.
	OutputFormat *string `gorm:"type:varchar(16)" json:"output_format,omitempty"`
}
