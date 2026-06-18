// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration parsed from environment variables.
type Config struct {
	// HTTP
	Port string // listen port, e.g. "3000"

	// PostgreSQL
	DBDSN string // full DSN; derived from POSTGRES_* if not provided directly

	// Redis
	RedisAddr     string // host:port, e.g. "localhost:6379"
	RedisPassword string
	RedisDB       int

	// Secrets
	JWTSecret string // HS256 signing secret for admin JWTs
	SecretKey string // AES-GCM key material for encrypting channel keys

	// Optional bootstrap admin (seeded on first start if both set)
	AdminUsername string
	AdminPassword string

	// Behaviour
	GinMode string // "debug" | "release" | "test"
}

// Load reads configuration from the environment and validates required fields.
//
// Required: JWT_SECRET, SECRET_KEY, and a database connection (either DB_DSN or
// the POSTGRES_* set), and a Redis address (REDIS_ADDR). Missing critical
// variables yield a clear aggregated error.
func Load() (*Config, error) {
	c := &Config{
		Port:          getEnv("PORT", "3000"),
		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		JWTSecret:     os.Getenv("JWT_SECRET"),
		SecretKey:     os.Getenv("SECRET_KEY"),
		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		GinMode:       getEnv("GIN_MODE", "release"),
	}

	if v := os.Getenv("REDIS_DB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: REDIS_DB must be an integer, got %q: %w", v, err)
		}
		c.RedisDB = n
	}

	c.DBDSN = buildDSN()

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// buildDSN returns DB_DSN verbatim when set, otherwise assembles a DSN from the
// POSTGRES_* family of variables (with sensible defaults).
func buildDSN() string {
	if dsn := os.Getenv("DB_DSN"); dsn != "" {
		return dsn
	}

	host := os.Getenv("POSTGRES_HOST")
	user := os.Getenv("POSTGRES_USER")
	password := os.Getenv("POSTGRES_PASSWORD")
	dbname := os.Getenv("POSTGRES_DB")
	// If none of the POSTGRES_* anchors are present, signal "unconfigured".
	if host == "" && user == "" && dbname == "" {
		return ""
	}

	host = orDefault(host, "localhost")
	port := getEnv("POSTGRES_PORT", "5432")
	user = orDefault(user, "postgres")
	dbname = orDefault(dbname, "agent_router")
	sslmode := getEnv("POSTGRES_SSLMODE", "disable")
	tz := getEnv("POSTGRES_TIMEZONE", "UTC")

	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		host, port, user, password, dbname, sslmode, tz,
	)
}

// validate aggregates all missing-critical-variable problems into one error.
func (c *Config) validate() error {
	var missing []string
	if c.JWTSecret == "" {
		missing = append(missing, "JWT_SECRET")
	}
	if c.SecretKey == "" {
		missing = append(missing, "SECRET_KEY (channel key encryption)")
	}
	if c.DBDSN == "" {
		missing = append(missing, "DB_DSN or POSTGRES_* (database connection)")
	}
	if c.RedisAddr == "" {
		missing = append(missing, "REDIS_ADDR")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if (c.AdminUsername == "") != (c.AdminPassword == "") {
		return errors.New("config: ADMIN_USERNAME and ADMIN_PASSWORD must be set together (or neither)")
	}
	return nil
}

// HasBootstrapAdmin reports whether a bootstrap admin should be seeded.
func (c *Config) HasBootstrapAdmin() bool {
	return c.AdminUsername != "" && c.AdminPassword != ""
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
