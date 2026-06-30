package model

import (
	"strconv"
	"time"

	"gorm.io/gorm"
)

// Option is a system configuration key/value entry.
type Option struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Key   string `gorm:"type:varchar(128);uniqueIndex;not null" json:"key"`
	Value string `gorm:"type:text" json:"value"`
}

// Well-known option keys seeded on first start (Tech Design §3 / §10).
const (
	OptDefaultUserQuota = "DefaultUserQuota"
	OptRegisterEnabled  = "RegisterEnabled"
	OptSystemName       = "SystemName"
	// OptRouterProbeMock — when "true" (default), the routing classifier is the
	// built-in deterministic Mock (no external call). The real SageMaker endpoint
	// integration is deferred; flipping this to "false" without a real probe wired
	// means w/t default to 0.
	OptRouterProbeMock = "RouterProbeMock"
	// OptRouterProbeEndpoint — the SageMaker endpoint name for the real classifier
	// (unused while mock is active; stored for the future real integration).
	OptRouterProbeEndpoint = "RouterProbeEndpoint"
)

// GetOption returns the value of an option key, or def if absent.
func GetOption(db *gorm.DB, key, def string) string {
	var opt Option
	if err := db.Where("key = ?", key).First(&opt).Error; err != nil {
		return def
	}
	return opt.Value
}

// DefaultUserQuota reads the DefaultUserQuota option as an int64 (0 on failure).
func DefaultUserQuota(db *gorm.DB) int64 {
	v := GetOption(db, OptDefaultUserQuota, "0")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// DefaultOptions returns the options seeded when the table is empty.
func DefaultOptions() []Option {
	return []Option{
		{Key: OptDefaultUserQuota, Value: "0"},
		{Key: OptRegisterEnabled, Value: "true"},
		{Key: OptSystemName, Value: "Agent Router Platform"},
	}
}
