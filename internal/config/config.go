package config

import (
	"log"
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

	// ClickHouseReadSource selects which store serves the raw-event
	// dashboard endpoints (recent / all-events / search / export / event
	// detail): "postgres" (default) or "clickhouse". It is separate from
	// ClickHouseEnabled because writing the mirror and reading from it are
	// different decisions — the mirror has no backfill, so for the first
	// days after it is enabled it holds less history than Postgres.
	// Overridable per request with ?store=clickhouse|postgres.
	ClickHouseReadSource string

	// --- Event storage backend ---
	//
	// EventStore selects where RAW events live: "postgres" (default) writes
	// them into the `events` table, "s3" writes them to an S3-compatible
	// object store (Backblaze B2, MinIO, AWS S3) and leaves the Postgres
	// table empty.
	//
	// Postgres remains the system of record either way: users, API keys,
	// sessions and the pre-aggregated rollup tables (metric_buckets,
	// route_buckets, daily_*) always live there, because they are small and
	// the dashboard reads them. Only the raw event stream — the part that
	// grows by millions of rows a day — moves.
	EventStore string

	// S3Endpoint is the S3-compatible endpoint URL, e.g.
	// https://s3.us-east-005.backblazeb2.com
	S3Endpoint string

	// S3Region is the signing region. Backblaze B2 reports it as part of the
	// endpoint (e.g. us-east-005); MinIO and most S3 clones accept anything.
	S3Region string

	// S3Bucket is the bucket that holds the event objects.
	S3Bucket string

	// S3AccessKeyID / S3SecretAccessKey are the S3 credentials. On B2 these
	// are the application key ID and the application key.
	S3AccessKeyID     string
	S3SecretAccessKey string

	// S3Prefix is the key prefix every object is written under. Default:
	// "events".
	S3Prefix string

	// S3PathStyle forces path-style addressing (bucket in the path rather
	// than the host). B2 and MinIO both want this; AWS S3 does not.
	// Default: true.
	S3PathStyle bool

	// S3FlushInterval is how long ingested events are buffered before being
	// written to one object per hour bucket. Longer means fewer, larger
	// objects and cheaper listing; shorter means fresher data. Default: 30s.
	S3FlushInterval time.Duration

	// S3MaxBufferEvents caps the in-memory buffer. Reaching it forces an
	// immediate flush so a burst cannot grow memory without bound.
	// Default: 200000.
	S3MaxBufferEvents int

	// S3StartupScanHours is how many completed hours the rollup worker
	// re-reads from the object store at startup. It exists because the
	// object store has no SQL, so a restart cannot recompute a missing hour
	// any other way. Default: 24 (matching the Postgres path).
	S3StartupScanHours int

	// S3ReadTimeout bounds one object read. Default: 30s.
	S3ReadTimeout time.Duration
}

// Event store names, as accepted by APP_EVENT_STORE.
const (
	EventStorePostgres = "postgres"
	EventStoreS3       = "s3"
)

// S3Enabled reports whether raw events are stored in the object store.
func (c *Config) S3Enabled() bool { return c.EventStore == EventStoreS3 }

// S3Configured reports whether every setting needed to open the object store
// is present. APP_EVENT_STORE=s3 without credentials is a misconfiguration,
// and the caller degrades to Postgres rather than starting broken.
func (c *Config) S3Configured() bool {
	return c.S3Bucket != "" && c.S3Endpoint != "" && c.S3AccessKeyID != "" && c.S3SecretAccessKey != ""
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
	cfg.ClickHouseReadSource = strings.ToLower(strings.TrimSpace(getenv("APP_CLICKHOUSE_READ_SOURCE", "postgres")))
	if cfg.ClickHouseReadSource != "clickhouse" {
		cfg.ClickHouseReadSource = "postgres"
	}

	// Event storage backend. Defaults to Postgres, so an existing deployment
	// that sets nothing behaves exactly as before.
	cfg.EventStore = strings.ToLower(strings.TrimSpace(getenv("APP_EVENT_STORE", EventStorePostgres)))
	if cfg.EventStore != EventStoreS3 {
		cfg.EventStore = EventStorePostgres
	}

	cfg.S3Endpoint = strings.TrimRight(strings.TrimSpace(os.Getenv("APP_S3_ENDPOINT")), "/")
	cfg.S3Region = strings.TrimSpace(getenv("APP_S3_REGION", "us-east-1"))
	cfg.S3Bucket = strings.TrimSpace(os.Getenv("APP_S3_BUCKET"))
	cfg.S3AccessKeyID = strings.TrimSpace(os.Getenv("APP_S3_ACCESS_KEY_ID"))
	cfg.S3SecretAccessKey = strings.TrimSpace(os.Getenv("APP_S3_SECRET_ACCESS_KEY"))
	cfg.S3Prefix = strings.Trim(strings.TrimSpace(getenv("APP_S3_PREFIX", "events")), "/")
	if cfg.S3Prefix == "" {
		cfg.S3Prefix = "events"
	}
	cfg.S3PathStyle = true
	if v := os.Getenv("APP_S3_PATH_STYLE"); v != "" {
		cfg.S3PathStyle = v == "true" || v == "1" || v == "yes"
	}
	cfg.S3FlushInterval = 30 * time.Second
	if d, ok := durationEnv("APP_S3_FLUSH_INTERVAL"); ok && d > 0 {
		cfg.S3FlushInterval = d
	}
	cfg.S3MaxBufferEvents = 200000
	if v := os.Getenv("APP_S3_MAX_BUFFER_EVENTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.S3MaxBufferEvents = n
		}
	}
	cfg.S3StartupScanHours = 24
	if v := os.Getenv("APP_S3_STARTUP_SCAN_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.S3StartupScanHours = n
		}
	}
	cfg.S3ReadTimeout = 30 * time.Second
	if d, ok := durationEnv("APP_S3_READ_TIMEOUT"); ok && d > 0 {
		cfg.S3ReadTimeout = d
	}

	// A half-configured object store is a mistake that would silently drop
	// every event once the process accepted it, so it is refused here and
	// the service keeps writing to Postgres instead.
	if cfg.EventStore == EventStoreS3 && !cfg.S3Configured() {
		log.Printf("event store: APP_EVENT_STORE=s3 but the bucket, endpoint or credentials are missing; " +
			"falling back to postgres so ingested events are not lost")
		cfg.EventStore = EventStorePostgres
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
