package config

import (
	"strings"
	"testing"
)

// clearEnv unsets every variable Load reads, so each test starts clean.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORT", "DB_DSN", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER",
		"POSTGRES_PASSWORD", "POSTGRES_DB", "POSTGRES_SSLMODE", "POSTGRES_TIMEZONE",
		"REDIS_ADDR", "REDIS_PASSWORD", "REDIS_DB", "JWT_SECRET", "SECRET_KEY",
		"ADMIN_USERNAME", "ADMIN_PASSWORD", "GIN_MODE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad_MissingCriticalVars(t *testing.T) {
	clearEnv(t)
	// No JWT_SECRET, SECRET_KEY, DB. REDIS_ADDR defaults so it's fine.
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing critical vars, got nil")
	}
	for _, want := range []string{"JWT_SECRET", "SECRET_KEY", "DB_DSN or POSTGRES_*"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing mention of %q", err.Error(), want)
		}
	}
}

func TestLoad_Success_WithDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "jwt-secret")
	t.Setenv("SECRET_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DB_DSN", "host=localhost user=postgres dbname=ar sslmode=disable")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "3000" {
		t.Errorf("default Port = %q, want 3000", cfg.Port)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("default RedisAddr = %q", cfg.RedisAddr)
	}
	if cfg.HasBootstrapAdmin() {
		t.Error("HasBootstrapAdmin should be false without ADMIN_* vars")
	}
}

func TestLoad_BuildsDSNFromPostgresVars(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "s")
	t.Setenv("SECRET_KEY", "k")
	t.Setenv("POSTGRES_HOST", "db")
	t.Setenv("POSTGRES_USER", "u")
	t.Setenv("POSTGRES_DB", "mydb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"host=db", "user=u", "dbname=mydb", "port=5432", "sslmode=disable"} {
		if !strings.Contains(cfg.DBDSN, want) {
			t.Errorf("DSN %q missing %q", cfg.DBDSN, want)
		}
	}
}

func TestLoad_AdminPairMustBeComplete(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "s")
	t.Setenv("SECRET_KEY", "k")
	t.Setenv("DB_DSN", "host=localhost")
	t.Setenv("ADMIN_USERNAME", "root")
	// ADMIN_PASSWORD intentionally absent.

	if _, err := Load(); err == nil {
		t.Fatal("expected error when only ADMIN_USERNAME is set")
	}
}

func TestLoad_InvalidRedisDB(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "s")
	t.Setenv("SECRET_KEY", "k")
	t.Setenv("DB_DSN", "host=localhost")
	t.Setenv("REDIS_DB", "notanint")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-integer REDIS_DB")
	}
}
