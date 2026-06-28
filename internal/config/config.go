package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds the core runtime configuration for the service.
// Values are primarily sourced from environment variables, with
// sensible defaults where appropriate. See .env.example.
type Config struct {
	AdminUser     string
	AdminPassword string

	DatabaseURL string

	// StatementTimeoutMs sets the maximum execution time for any SQL statement.
	// 0 (default) means no timeout. Recommended: 30000 (30s).
	StatementTimeoutMs int

	// RetentionDays is the maximum retention (in days) that any individual
	// API key is allowed to request. Per-key settings will be clamped to
	// this value.
	RetentionDays int

	ListenAddr string

	// InternalAPIKey is used for self-reporting metrics from this API Insight instance.
	// If empty, internal reporting is disabled.
	InternalAPIKey string

	// SessionSecret signs dashboard session cookies.
	SessionSecret string

	// LogLevel sets the minimum logging level: debug, info, warn, error, fatal.
	// Default: info.
	LogLevel string

	// PrettyLog enables human-readable console output instead of JSON lines.
	// Default: false (JSON output).
	PrettyLog bool
}

// Load reads configuration from environment variables and applies
func Load() *Config {
	cfg := &Config{
		AdminUser:          getenv("APP_ADMIN_USER", "admin"),
		AdminPassword:      getenv("APP_ADMIN_PASSWORD", "changeme"),
		DatabaseURL:        os.Getenv("APP_DATABASE_URL"),
		ListenAddr:         getenv("APP_LISTEN_ADDR", ":8080"),
		RetentionDays:      30,
		InternalAPIKey:     getenv("APP_INTERNAL_API_KEY", ""),
		StatementTimeoutMs: 120000,
	}

	if v := os.Getenv("APP_STATEMENT_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.StatementTimeoutMs = n
		}
	}

	if v := os.Getenv("APP_RETENTION_DAYS"); v != "" {
		if days, err := strconv.Atoi(v); err == nil && days > 0 {
			cfg.RetentionDays = days
		}
	}
	cfg.LogLevel = strings.TrimSpace(os.Getenv("APP_LOG_LEVEL"))
	if v := os.Getenv("APP_PRETTY_LOG"); v != "" {
		cfg.PrettyLog = v == "true" || v == "1" || v == "yes"
	}
	cfg.SessionSecret = strings.TrimSpace(os.Getenv("APP_SESSION_SECRET"))
	if cfg.SessionSecret == "" {
		// Keep existing installs working, but prefer APP_SESSION_SECRET in production.
		if cfg.InternalAPIKey != "" {
			cfg.SessionSecret = cfg.InternalAPIKey
		} else {
			cfg.SessionSecret = cfg.AdminPassword
		}
	}

	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}