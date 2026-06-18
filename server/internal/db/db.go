// Package db provides PostgreSQL and Redis connection helpers.
package db

import (
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/agent-router/server/internal/model"
)

// Connect opens a GORM PostgreSQL connection using the given DSN and verifies
// connectivity with a ping. A connection failure returns an error so the caller
// can abort startup.
func Connect(dsn string) (*gorm.DB, error) {
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Warn),
		SkipDefaultTransaction: true,
		NowFunc:                func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: access sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("db: ping postgres: %w", err)
	}
	return gdb, nil
}

// AutoMigrate creates/updates all model tables and indexes.
func AutoMigrate(gdb *gorm.DB) error {
	if err := model.Migrate(gdb); err != nil {
		return fmt.Errorf("db: automigrate: %w", err)
	}
	return nil
}
