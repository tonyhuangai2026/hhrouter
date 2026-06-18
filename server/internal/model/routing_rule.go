package model

import (
	"time"

	"gorm.io/datatypes"
)

// RoutingRule defines how relay requests are matched to candidate channels.
type RoutingRule struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Name    string `gorm:"type:varchar(128);not null" json:"name"`
	Enabled bool   `gorm:"not null;default:true;index" json:"enabled"`
	// Priority: lower matches first (ascending order).
	Priority int `gorm:"not null;default:0;index" json:"priority"`

	// Match is the matching predicate (JSONB), e.g.:
	//   { "groups": ["vip"], "models": ["gpt-4o","claude-*"],
	//     "min_tokens": 0, "max_tokens": 32000 }
	// Any empty field means that dimension is unconstrained; models supports
	// "*" wildcards; min/max_tokens apply to the estimated input tokens.
	Match datatypes.JSON `gorm:"type:jsonb" json:"match"`

	// TargetChannelIDs is the candidate channel set (JSONB int array).
	TargetChannelIDs datatypes.JSON `gorm:"type:jsonb" json:"target_channel_ids"`

	// TargetGroup may be used instead of TargetChannelIDs to point at a channel
	// group (nullable).
	TargetGroup string `gorm:"type:varchar(64)" json:"target_group,omitempty"`
}

// MatchSpec is the decoded shape of RoutingRule.Match.
type MatchSpec struct {
	Groups    []string `json:"groups,omitempty"`
	Models    []string `json:"models,omitempty"`
	MinTokens int      `json:"min_tokens,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
}
