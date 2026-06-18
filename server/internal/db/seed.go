package db

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// SeedDefaultOptions inserts the well-known default options if they are not
// already present. It is idempotent across restarts.
func SeedDefaultOptions(gdb *gorm.DB) error {
	for _, opt := range model.DefaultOptions() {
		var count int64
		if err := gdb.Model(&model.Option{}).Where("key = ?", opt.Key).Count(&count).Error; err != nil {
			return fmt.Errorf("db: seed options: %w", err)
		}
		if count == 0 {
			if err := gdb.Create(&opt).Error; err != nil {
				return fmt.Errorf("db: seed option %q: %w", opt.Key, err)
			}
		}
	}
	return nil
}

// SeedAdmin creates a bootstrap admin user from env if one does not already
// exist with the given username. The password is stored as a bcrypt hash. It is
// a no-op when username/password are empty. Returns true if a user was created.
func SeedAdmin(gdb *gorm.DB, username, password string) (bool, error) {
	if username == "" || password == "" {
		return false, nil
	}

	var existing model.User
	err := gdb.Where("username = ?", username).First(&existing).Error
	switch {
	case err == nil:
		return false, nil // already seeded
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return false, fmt.Errorf("db: lookup admin %q: %w", username, err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, fmt.Errorf("db: hash admin password: %w", err)
	}

	admin := model.User{
		Username:    username,
		Password:    string(hash),
		DisplayName: "Administrator",
		Role:        model.RoleAdmin,
		Status:      model.UserEnabled,
		Quota:       model.DefaultUserQuota(gdb),
	}
	if err := gdb.Create(&admin).Error; err != nil {
		return false, fmt.Errorf("db: create admin %q: %w", username, err)
	}
	return true, nil
}
