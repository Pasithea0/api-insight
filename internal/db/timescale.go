// Package db — TimescaleDB integration.
//
// api-insight stores one row per ingested API request in `events`. At
// ~13M rows/day that table grows without bound, every dashboard query
// degrades, and retention deletes bloat the table and need VACUUM.
//
// When the connected Postgres has the TimescaleDB extension, this file
// converts `events` into a hypertable, which gives us three things the
// hand-rolled version cannot:
//
//   - time partitioning: data is split into per-chunk tables, so queries
//     that touch one hour only read one chunk;
//   - columnstore (compressed) chunks: old chunks are converted to a
//     columnar layout, cutting storage and speeding up scans;
//   - chunk-drop retention: expiring old data is a metadata operation
//     instead of a mass DELETE + VACUUM.
//
// Everything here is a no-op when the extension is not installed, so a
// plain Postgres deployment keeps working exactly as before.
//
// The SQL in this file was validated against TimescaleDB 2.30.1 / PG 17.
// Behaviours that are easy to get wrong, all confirmed empirically:
//
//   - A hypertable's PRIMARY KEY must include the partitioning column, so
//     `events` needs PRIMARY KEY (id, created_at) before it can be
//     converted. Without that, create_hypertable() fails.
//   - `CREATE INDEX CONCURRENTLY` is NOT supported on hypertables
//     ("hypertables do not support concurrent index creation"), so index
//     creation has to take the plain path once `events` is a hypertable.
//   - `add_columnstore_policy` and `convert_to_columnstore` are
//     PROCEDURES, not functions, so they must be invoked with CALL.
//   - A chunk's name is only resolvable when qualified with its schema
//     (`_timescaledb_internal`), hence the format('%I.%I', ...) casts.
//   - percentile_cont() IS accepted inside a continuous aggregate on 2.30
//     and matches ground truth, which is what lets the rollup worker read
//     pre-computed percentiles instead of scanning raw rows.
package db

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	// defaultChunkInterval is used when no chunk interval is configured.
	defaultChunkInterval = 24 * time.Hour

	// continuousAggregateMetrics and continuousAggregateRoutes are the
	// materialised hourly rollups created inside the database.
	continuousAggregateMetrics = "events_hourly"
	continuousAggregateRoutes  = "events_hourly_routes"
)

// TimescaleOptions controls how far the hypertable setup goes.
type TimescaleOptions struct {
	// Enabled gates the whole feature. It is a no-op without the extension.
	Enabled bool

	// ChunkInterval is the time span covered by each hypertable chunk.
	ChunkInterval time.Duration

	// ColumnstoreAfter is the age at which a chunk is moved to columnstore
	// storage. Zero disables columnstore.
	ColumnstoreAfter time.Duration

	// RawRetentionDays adds a chunk-drop based hard age cap on raw events.
	// Zero disables it.
	RawRetentionDays int

	// ContinuousAggregates materialises hourly rollups so the aggregation
	// worker can read pre-computed rows instead of scanning `events`.
	ContinuousAggregates bool
}

// TimescaleState reports what the last setup pass found or changed. It is
// used by /healthz and by the aggregation worker to decide whether it can
// read from continuous aggregates.
type TimescaleState struct {
	// Available is true when the timescaledb extension is installed.
	Available bool
	// Version is the installed extension version, e.g. "2.30.1".
	Version string
	// IsHypertable is true when `events` is a hypertable.
	IsHypertable bool
	// ChunkCount is the number of chunks currently backing `events`.
	ChunkCount int64
	// ColumnstoreEnabled is true when chunks are configured to compress.
	ColumnstoreEnabled bool
	// ContinuousAggregates is true when the hourly rollup caggs exist.
	ContinuousAggregates bool
	// ChunkInterval is the interval in effect (for logging/diagnostics).
	ChunkInterval time.Duration
}

// tsState is the process-wide result of setupTimescale. The aggregation
// worker reads it to pick its source relation, and healthz reports it.
var tsState TimescaleState

// TimescaleStatus returns a copy of the last known TimescaleDB state.
func TimescaleStatus() TimescaleState {
	return tsState
}

// UseContinuousAggregates reports whether the rollup worker should read
// pre-computed hourly rollups rather than raw events.
func UseContinuousAggregates() bool {
	return tsState.ContinuousAggregates
}

// setupTimescale runs every TimescaleDB step in order. Any failure short of
// "the extension is not installed" is returned so startup can report it,
// but the caller should treat a missing extension as a normal condition.
func setupTimescale(gdb *gorm.DB, opts TimescaleOptions) (TimescaleState, error) {
	var st TimescaleState
	if !opts.Enabled {
		return st, nil
	}

	available, version := timescaleExtensionAvailable(gdb)
	st.Available = available
	st.Version = version
	if !available {
		log.Println("timescale: extension not available on this Postgres, continuing without it")
		return st, nil
	}

	st.ChunkInterval = opts.ChunkInterval
	if st.ChunkInterval <= 0 {
		st.ChunkInterval = defaultChunkInterval
	}

	if err := ensureHypertable(gdb, st.ChunkInterval); err != nil {
		return st, fmt.Errorf("ensure hypertable: %w", err)
	}
	st.IsHypertable = true

	if err := configureColumnstore(gdb, opts.ColumnstoreAfter); err != nil {
		// Columnstore is an optimisation; failing it must not stop ingest.
		log.Printf("timescale: columnstore setup failed (continuing uncompressed): %v", err)
	} else {
		st.ColumnstoreEnabled = opts.ColumnstoreAfter > 0
	}

	if opts.RawRetentionDays > 0 {
		if err := ensureRetentionPolicy(gdb, opts.RawRetentionDays); err != nil {
			log.Printf("timescale: retention policy setup failed: %v", err)
		}
	}

	if opts.ContinuousAggregates {
		if err := ensureContinuousAggregates(gdb); err != nil {
			// Without caggs the rollup worker falls back to raw events.
			log.Printf("timescale: continuous aggregate setup failed (rollups will scan raw events): %v", err)
		} else {
			st.ContinuousAggregates = true
		}
	}

	st.ChunkCount = hypertableChunkCount(gdb)

	log.Printf("timescale: ready (v%s, hypertable=%t, chunks=%d, columnstore=%t, caggs=%t, chunk_interval=%s)",
		st.Version, st.IsHypertable, st.ChunkCount, st.ColumnstoreEnabled,
		st.ContinuousAggregates, st.ChunkInterval)

	return st, nil
}

// timescaleExtensionAvailable reports whether timescaledb can be used, by
// checking pg_available_extensions rather than pg_extension: on a fresh
// database the extension exists but has not been created yet, and only the
// former distinguishes "supported" from "installed".
func timescaleExtensionAvailable(gdb *gorm.DB) (bool, string) {
	var row struct {
		Version string
	}
	err := gdb.Raw(`
		SELECT COALESCE(e.extversion, a.default_version) AS version
		FROM pg_available_extensions a
		LEFT JOIN pg_extension e ON e.extname = a.name
		WHERE a.name = 'timescaledb'
	`).Scan(&row).Error
	if err != nil || row.Version == "" {
		return false, ""
	}
	return true, row.Version
}

// ensureHypertable creates the extension, makes the primary key
// hypertable-compatible, and converts `events`.
//
// The primary key change is the expensive part: TimescaleDB requires every
// unique index on a hypertable to include the partitioning column, so
// `events` must move from PRIMARY KEY (id) to PRIMARY KEY (id, created_at).
// That rebuilds the PK index under an ACCESS EXCLUSIVE lock. It is done
// once, and only when the current PK is not already composite.
func ensureHypertable(gdb *gorm.DB, chunkInterval time.Duration) error {
	if err := gdb.Exec("CREATE EXTENSION IF NOT EXISTS timescaledb").Error; err != nil {
		// Typically a permissions problem: CREATE EXTENSION needs
		// superuser or a pre-created extension. Report, don't panic.
		return fmt.Errorf("create extension: %w", err)
	}

	if isHypertable(gdb, "events") {
		return nil
	}

	if err := ensureCompositePrimaryKey(gdb); err != nil {
		return err
	}

	// migrate_data => true moves existing rows into chunks. That is a
	// lock-heavy one-off on a large table; it is required because the
	// alternative leaves all current rows in the parent table.
	sql := fmt.Sprintf(
		"SELECT create_hypertable('events', 'created_at', chunk_time_interval => INTERVAL '%d seconds', migrate_data => true, if_not_exists => true)",
		int64(chunkInterval.Seconds()),
	)
	if err := gdb.Exec(sql).Error; err != nil {
		return fmt.Errorf("create_hypertable: %w", err)
	}
	log.Printf("timescale: converted events to a hypertable (chunk interval %s)", chunkInterval)
	return nil
}

// ensureCompositePrimaryKey rewrites events' primary key to (id, created_at)
// when it is currently just (id). It is idempotent.
func ensureCompositePrimaryKey(gdb *gorm.DB) error {
	var cols []string
	if err := gdb.Raw(`
		SELECT a.attname
		FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = 'events'::regclass AND i.indisprimary
		ORDER BY a.attname
	`).Scan(&cols).Error; err != nil {
		return fmt.Errorf("inspect primary key: %w", err)
	}

	hasID, hasCreatedAt := false, false
	for _, c := range cols {
		switch c {
		case "id":
			hasID = true
		case "created_at":
			hasCreatedAt = true
		}
	}
	if hasID && hasCreatedAt {
		return nil // already hypertable-compatible
	}
	if len(cols) == 0 {
		// No PK at all: add the required one rather than assuming.
		log.Println("timescale: events has no primary key; adding (id, created_at)")
		return gdb.Exec("ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)").Error
	}

	log.Println("timescale: rewriting events primary key to (id, created_at) — this rebuilds the PK index under an exclusive lock")
	return gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE events DROP CONSTRAINT IF EXISTS events_pkey").Error; err != nil {
			return fmt.Errorf("drop old primary key: %w", err)
		}
		if err := tx.Exec("ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)").Error; err != nil {
			return fmt.Errorf("add composite primary key: %w", err)
		}
		return nil
	})
}

// configureColumnstore enables columnstore storage on `events` and, when
// after > 0, installs the policy that moves chunks into it automatically.
//
// `after` should be longer than the longest per-key retention window: rows
// in a columnstore chunk can still be deleted (verified — TimescaleDB
// decompresses as needed), but it is far cheaper to let a chunk age out
// whole than to delete individual rows from it.
func configureColumnstore(gdb *gorm.DB, after time.Duration) error {
	if after <= 0 {
		return nil
	}
	if !isHypertable(gdb, "events") {
		return errors.New("events is not a hypertable")
	}

	// enable_columnstore is the 2.18+ API; the older timescaledb.compress
	// spelling still works as an alias but is deprecated.
	err := gdb.Exec(`
		ALTER TABLE events SET (
			timescaledb.enable_columnstore = true,
			timescaledb.segmentby = 'user_id,project',
			timescaledb.orderby = 'created_at DESC'
		)`).Error
	if err != nil {
		return fmt.Errorf("enable columnstore: %w", err)
	}

	// add_columnstore_policy is a PROCEDURE on 2.30; SELECT ... would fail
	// with "is a procedure, use CALL".
	sql := fmt.Sprintf(
		"CALL add_columnstore_policy('events', after => INTERVAL '%d seconds', if_not_exists => true)",
		int64(after.Seconds()),
	)
	if err := gdb.Exec(sql).Error; err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("add columnstore policy: %w", err)
	}
	log.Printf("timescale: columnstore enabled, chunks compress after %s", after)
	return nil
}

// ensureRetentionPolicy installs a chunk-drop retention policy, the hard
// age cap on raw events. Per-API-key retention keeps working through the
// row-level deletion worker; this policy only enforces the global ceiling,
// which is dramatically cheaper than deleting row by row.
func ensureRetentionPolicy(gdb *gorm.DB, days int) error {
	if !isHypertable(gdb, "events") {
		return errors.New("events is not a hypertable")
	}
	sql := fmt.Sprintf(
		"SELECT add_retention_policy('events', drop_after => INTERVAL '%d days', if_not_exists => true)",
		days,
	)
	if err := gdb.Exec(sql).Error; err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("add retention policy: %w", err)
	}
	log.Printf("timescale: chunk-drop retention policy set to %d days", days)
	return nil
}

// ensureContinuousAggregates creates the hourly rollups the dashboard
// buckets are built from. They mirror exactly the two shapes the rollup
// worker needs, so the worker keeps its existing Go logic and only swaps
// its FROM clause.
//
// Column names deliberately match the existing MetricBucket / RouteBucket
// fields to keep that switch mechanical.
func ensureContinuousAggregates(gdb *gorm.DB) error {
	if !isHypertable(gdb, "events") {
		return errors.New("events is not a hypertable")
	}

	metricsSQL := fmt.Sprintf(`
		CREATE MATERIALIZED VIEW IF NOT EXISTS %s
		WITH (timescaledb.continuous) AS
		SELECT
			time_bucket(INTERVAL '1 hour', created_at) AS bucket_start,
			user_id,
			project,
			count(*)                                            AS total_count,
			count(*) FILTER (WHERE status >= 400)               AS error_count,
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms) AS p50,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95,
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) AS p99
		FROM events
		GROUP BY bucket_start, user_id, project
		WITH NO DATA`, continuousAggregateMetrics)

	routesSQL := fmt.Sprintf(`
		CREATE MATERIALIZED VIEW IF NOT EXISTS %s
		WITH (timescaledb.continuous) AS
		SELECT
			time_bucket(INTERVAL '1 hour', created_at) AS bucket_start,
			user_id,
			project,
			route,
			status,
			count(*)             AS count,
			avg(duration_ms)     AS avg_duration
		FROM events
		GROUP BY bucket_start, user_id, project, route, status
		WITH NO DATA`, continuousAggregateRoutes)

	for _, sql := range []string{metricsSQL, routesSQL} {
		if err := gdb.Exec(sql).Error; err != nil {
			if isAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("create continuous aggregate: %w", err)
		}
	}

	// Refresh policies so the caggs stay current even if the worker is
	// down. The worker additionally refreshes the exact bucket it is about
	// to aggregate, which is what makes its output deterministic.
	for _, name := range []string{continuousAggregateMetrics, continuousAggregateRoutes} {
		sql := fmt.Sprintf(`
			SELECT add_continuous_aggregate_policy('%s',
				start_offset => INTERVAL '90 days',
				end_offset   => INTERVAL '1 hour',
				schedule_interval => INTERVAL '10 minutes',
				if_not_exists => true)`, name)
		if err := gdb.Exec(sql).Error; err != nil && !isAlreadyExists(err) {
			log.Printf("timescale: could not add refresh policy for %s: %v", name, err)
		}
	}

	log.Println("timescale: continuous aggregates ready (hourly metrics + routes)")
	return nil
}

// refreshRollupAggregates materialises the caggs for [start, end) so the
// rollup worker reads committed data rather than relying on the background
// refresh schedule.
func refreshRollupAggregates(gdb *gorm.DB, start, end time.Time) error {
	if !tsState.ContinuousAggregates {
		return nil
	}
	for _, name := range []string{continuousAggregateMetrics, continuousAggregateRoutes} {
		// Explicit casts are required: Postgres cannot infer parameter types
		// inside CALL the way it does for a SELECT, and without them this
		// fails with "could not determine data type of parameter $2".
		if err := gdb.Exec(
			"CALL refresh_continuous_aggregate(?::text::regclass, ?::timestamptz, ?::timestamptz)",
			name, start, end,
		).Error; err != nil {
			return fmt.Errorf("refresh %s: %w", name, err)
		}
	}
	return nil
}

// isHypertable reports whether the given table has been converted.
func isHypertable(gdb *gorm.DB, table string) bool {
	var count int64
	err := gdb.Raw(`
		SELECT COUNT(*) FROM timescaledb_information.hypertables
		WHERE hypertable_name = ?`, table).Scan(&count).Error
	if err != nil {
		// Extension missing, or the view does not exist: not a hypertable.
		return false
	}
	return count > 0
}

// hypertableChunkCount returns the current chunk count, for diagnostics.
func hypertableChunkCount(gdb *gorm.DB) int64 {
	var count int64
	_ = gdb.Raw(`
		SELECT COUNT(*) FROM timescaledb_information.chunks
		WHERE hypertable_name = 'events'`).Scan(&count).Error
	return count
}

// isAlreadyExists recognises the several ways TimescaleDB reports "this
// policy/object is already present", so idempotent setup stays quiet.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"already exists",
		"already has a",
		"duplicate object",
		"if_not_exists",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
