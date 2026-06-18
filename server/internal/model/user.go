// Package model defines the GORM models and migration entry point for the
// Agent Router Platform, per Tech Design §3.
package model

import "time"

// UserRole enumerates the access level of a user account.
type UserRole string

const (
	// RoleAdmin has full administrative access. The first registered user
	// becomes an admin.
	RoleAdmin UserRole = "admin"
	// RoleUser is a regular, non-administrative account.
	RoleUser UserRole = "user"
)

// UserStatus enumerates whether an account may authenticate.
type UserStatus string

const (
	UserEnabled  UserStatus = "enabled"
	UserDisabled UserStatus = "disabled"
)

// User is a unified account for both the admin console (JWT) and quota tracking.
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Username    string `gorm:"type:varchar(64);uniqueIndex;not null" json:"username"`
	Password    string `gorm:"type:varchar(255);not null" json:"-"` // bcrypt hash, never serialized
	DisplayName string `gorm:"type:varchar(128)" json:"display_name"`
	Email       string `gorm:"type:varchar(255)" json:"email,omitempty"` // nullable

	Role   UserRole   `gorm:"type:varchar(16);not null;default:'user'" json:"role"`
	Status UserStatus `gorm:"type:varchar(16);not null;default:'enabled'" json:"status"`

	// Quota / UsedQuota are in micro-USD (1 USD = 1_000_000). Billing is USD-based:
	// the relay computes each request's cost from the (channel, model) price and
	// debits UsedQuota by that micro-USD amount. (Historically these were token
	// counts; the unit changed with USD billing — values are not auto-converted.)
	Quota     int64 `gorm:"not null;default:0" json:"quota"`      // upper bound, micro-USD
	UsedQuota int64 `gorm:"not null;default:0" json:"used_quota"` // consumed, micro-USD

	// Group is the default routing group applied to tokens this user creates
	// when the create request does not specify one. AutoMigrate adds this as a
	// NOT NULL column with a 'default' default, so existing rows backfill to
	// 'default' without a destructive migration.
	Group string `gorm:"type:varchar(64);not null;default:'default';index" json:"group"`

	// LastLoginAt records the most recent successful login. It is nullable
	// (NULL until the user logs in for the first time after this column exists)
	// and updated best-effort on each successful Login.
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}
