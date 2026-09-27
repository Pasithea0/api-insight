package config

import (
	"os"
	"strconv"
	"strings"
	"time"
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

	// --- TimescaleDB (optional) ---
	//
	// TimescaleEnabled turns the `events` table into a TimescaleDB
	// hypertable (time-partitioned chunks, columnstore compression,
	// chunk-drop retention, continuous aggregates). It is a no-op when
	// the connected Postgres does not have the timescaledb extension
	// available, so plain Postgres deployments keep working unchanged.
	// Default: true.
	TimescaleEnabled bool

	// TimescaleChunkInterval is how much time each hypertable chunk covers.
	// Smaller chunks make retention and compression cheaper but increase
	// planning overhead. Default: 24h.
	TimescaleChunkInterval time.Duration

	// TimescaleColumnstoreAfter is the age at which a chunk is moved to
	// columnstore (compressed) storage. 0 disables columnstore entirely.
	// Must be greater than the longest per-key retention window, because
	// deleting individual rows from columnstore chunks is expensive.
	// Default: 14 days.
	TimescaleColumnstoreAfter time.Duration

	// RawRetentionDays is a hard upper bound on the age of raw events,
	// enforced by TimescaleDB's chunk-drop retention policy. It is an
	// additional floor on top of per-API-key retention, not a replacement:
	// a key may request fewer days, never more. 0 disables it.
	RawRetentionDays int

	// TimescaleContinuousAggregates materialises hourly rollups inside the
	// database (percentile_cont is supported in caggs on TimescaleDB 2.30),
	// so the aggregation worker reads pre-computed rows instead of scanning
	// raw events. Default: true.
	TimescaleContinuousAggregates bool

	// --- ClickHouse (optional) ---
	//
	// ClickHouseEnabled mirrors every ingested event into ClickHouse in
	// addition to Postgres. Postgres remains the system of record for
	// ownership, API keys and the dashboard rollups; ClickHouse is a wide,
	// cheap, columnar copy for long-range ad-hoc queries and search.
	// Default: false.
	ClickHouseEnabled bool

	// ClickHouseDSN is a clickhouse:// DSN, e.g.
	// clickhouse://user:pass@host:9000/database
	ClickHouseDSN string

	// ClickHouseRetentionDays is the additional global age TTL applied to
	// the ClickHouse events table. 0 means rely on per-row expires_at only.
	ClickHouseRetentionDays int
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

		// TimescaleDB is on by default and self-disables when the
		// extension is unavailable, so this stays safe on plain Postgres.
		TimescaleEnabled:              true,
		TimescaleChunkInterval:        24 * time.Hour,
		TimescaleColumnstoreAfter:     14 * 24 * time.Hour,
		TimescaleContinuousAggregates: true,
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

	// TimescaleDB tuning. Defaults keep the feature on but harmless: every
	// step below is a no-op when the extension is not installed, so a plain
	// Postgres instance behaves exactly as it did before.
	if v := os.Getenv("APP_TIMESCALE_ENABLED"); v != "" {
		cfg.TimescaleEnabled = v == "true" || v == "1" || v == "yes"
	}
	if d, ok := durationEnv("APP_TIMESCALE_CHUNK_INTERVAL"); ok {
		cfg.TimescaleChunkInterval = d
	}
	if d, ok := durationEnv("APP_TIMESCALE_COLUMNSTORE_AFTER"); ok {
		cfg.TimescaleColumnstoreAfter = d
	}
	if v := os.Getenv("APP_TIMESCALE_CONTINUOUS_AGGREGATES"); v != "" {
		cfg.TimescaleContinuousAggregates = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("APP_RAW_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RawRetentionDays = n
		}
	}

	// ClickHouse mirroring. Only ever on when a DSN is explicitly set, so
	// flipping APP_CLICKHOUSE_ENABLED alone cannot half-enable the sink.
	cfg.ClickHouseDSN = strings.TrimSpace(os.Getenv("APP_CLICKHOUSE_DSN"))
	if v := os.Getenv("APP_CLICKHOUSE_ENABLED"); v != "" {
		cfg.ClickHouseEnabled = v == "true" || v == "1" || v == "yes"
	}
	if cfg.ClickHouseEnabled && cfg.ClickHouseDSN == "" {
		cfg.ClickHouseEnabled = false
	}
	if v := os.Getenv("APP_CLICKHOUSE_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.ClickHouseRetentionDays = n
		}
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

// durationEnv reads a duration from the environment. It accepts either a Go
// duration string ("36h", "90m") or a bare integer, which is interpreted as a
// number of days ("14" == 14 days) because the values these knobs control are
// almost always expressed in days. A negative duration is rejected so a typo
// can never produce a policy that runs backwards.
func durationEnv(key string) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}
