package model

import (
	"time"

	"gorm.io/datatypes"
)

// ChannelType enumerates the kind of upstream a channel proxies to.
type ChannelType string

const (
	// ChannelOpenAI forwards to an OpenAI-compatible upstream.
	ChannelOpenAI ChannelType = "openai"
	// ChannelBedrock forwards to AWS Bedrock runtime via the Converse API.
	ChannelBedrock ChannelType = "bedrock"
	// ChannelAnthropic forwards to the native Anthropic Messages API
	// (POST {base_url}/v1/messages with x-api-key + anthropic-version headers).
	ChannelAnthropic ChannelType = "anthropic"
)

// ChannelStatus enumerates the operational state of a channel.
type ChannelStatus string

const (
	ChannelEnabled      ChannelStatus = "enabled"
	ChannelDisabled     ChannelStatus = "disabled"
	ChannelAutoDisabled ChannelStatus = "auto_disabled"
)

// Channel is an upstream provider endpoint that relay requests can be routed to.
type Channel struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Name string      `gorm:"type:varchar(128);not null" json:"name"`
	Type ChannelType `gorm:"type:varchar(16);not null" json:"type"`

	// BaseURL: for openai, the upstream address (e.g. https://api.openai.com).
	// For bedrock it is left empty; the endpoint is derived from Region.
	BaseURL string `gorm:"type:varchar(255)" json:"base_url"`

	// Key is the upstream credential. For openai this is the upstream sk-key;
	// for bedrock the Bedrock Bearer API key. Stored AES-GCM encrypted (the
	// encryption key comes from env SECRET_KEY). Never serialized to JSON.
	Key string `gorm:"type:text" json:"-"`

	// Region is used by bedrock channels (e.g. us-east-1).
	Region string `gorm:"type:varchar(32)" json:"region,omitempty"`

	// Models is the list of available model ids (JSONB string array).
	Models datatypes.JSON `gorm:"type:jsonb" json:"models"`

	// ModelMapping optionally maps an external model name to the upstream's
	// real model id (JSONB object).
	ModelMapping datatypes.JSON `gorm:"type:jsonb" json:"model_mapping,omitempty"`

	// UseInferenceProfile, when true (default), makes the bedrock adapter
	// auto-prefix bare provider model ids (anthropic./amazon./meta./...) with the
	// channel region's cross-region inference-profile group (us./eu./apac.) at
	// invoke time. Ids that already carry a region-group prefix, or values set via
	// model_mapping that are already prefixed, are left unchanged. Only consulted
	// by the bedrock adapter; ignored for openai channels.
	UseInferenceProfile bool `gorm:"not null;default:true" json:"use_inference_profile"`

	// Group is the routing group tag used to group channels for key routing.
	Group string `gorm:"type:varchar(64);not null;default:'default';index" json:"group"`

	Priority int `gorm:"not null;default:0" json:"priority"` // higher = preferred
	Weight   int `gorm:"not null;default:1" json:"weight"`   // weighted random within a priority bucket

	Status ChannelStatus `gorm:"type:varchar(16);not null;default:'enabled';index" json:"status"`
}
