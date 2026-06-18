package model

import "gorm.io/gorm"

// AllModels returns every model registered for AutoMigrate, in dependency order.
func AllModels() []any {
	return []any{
		&User{},
		&Channel{},
		&Token{},
		&RoutingRule{},
		&RequestLog{},
		&ModelPrice{},
		&Option{},
	}
}

// Migrate runs GORM AutoMigrate for all models, creating the
// users/channels/tokens/routing_rules/request_logs/options tables and their
// declared indexes. It then runs idempotent post-migration fixups for schema
// changes that AutoMigrate cannot perform on an already-populated table.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return err
	}
	return postMigrate(db)
}

// postMigrate applies idempotent schema fixups that GORM's AutoMigrate does not
// handle for existing tables.
//
// RequestLog.TokenID was originally `uint NOT NULL`; it is now `*uint`
// (nullable) so admin test-chat traffic can write token_id=NULL. On a FRESH
// database AutoMigrate creates the column nullable, but on an UPGRADED database
// AutoMigrate adds columns/indexes yet does NOT relax an existing NOT NULL
// constraint — leaving request_logs.token_id NOT NULL and causing test-chat
// inserts (token_id=NULL) to fail. We therefore explicitly drop the constraint.
//
// The statement runs only on PostgreSQL (the production dialect; SQLite unit
// tests create the column nullable from birth and need no fixup) and only when
// the column currently has a NOT NULL constraint, so it is a safe no-op on
// fresh databases and on every subsequent boot. It tolerates the table not yet
// existing.
func postMigrate(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}

	// Guard on information_schema so we only ALTER when the column exists and is
	// still NOT NULL. Dropping NOT NULL on an already-nullable column is itself a
	// no-op in Postgres, but the guard also covers the table-missing case and
	// avoids issuing a needless DDL on every boot.
	var needsDrop bool
	if err := db.Raw(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'request_logs'
			  AND column_name = 'token_id'
			  AND is_nullable = 'NO'
		)`,
	).Scan(&needsDrop).Error; err != nil {
		return err
	}
	if !needsDrop {
		return nil
	}

	return db.Exec(`ALTER TABLE request_logs ALTER COLUMN token_id DROP NOT NULL`).Error
}
