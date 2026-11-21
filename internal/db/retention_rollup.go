package db

import (
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
)

// RollupWindows says how long the pre-aggregated tables are kept.
//
// They are pruned independently of the raw-event policy because they live in
// Postgres whatever the event store is. Moving events to an object store took
// the raw data off the expensive disk and left these behind, and nothing
// pruned them: one deployment reached 358 million rows and 127 GB in
// route_buckets alone — 2.6x the raw event table it summarises, and 100% of
// what remained after the events were cleared.
//
// Zero means keep forever, the same reading APP_RETENTION_DAYS uses.
type RollupWindows struct {
	// HourlyDays governs route_buckets and metric_buckets.
	HourlyDays int
	// DailyDays governs daily_route_buckets and daily_metric_buckets, which
	// are the long-range record: the dashboard falls back to them for ranges
	// beyond 48h.
	DailyDays int
}

// rollupTable describes one pre-aggregated table: its name and the column that
// records how old a row is.
type rollupTable struct {
	name   string
	ageCol string
	hourly bool
}

// rollupTables is the single list of pre-aggregated tables.
//
// It exists so that adding a rollup to the aggregation worker and forgetting
// it here is a visible omission rather than a silent one. That was precisely
// the old failure mode: a table nobody pruned, growing until the disk filled,
// with no error anywhere to say so.
var rollupTables = []rollupTable{
	{name: "route_buckets", ageCol: "bucket_start", hourly: true},
	{name: "metric_buckets", ageCol: "bucket_start", hourly: true},
	{name: "daily_route_buckets", ageCol: "bucket_date", hourly: false},
	{name: "daily_metric_buckets", ageCol: "bucket_date", hourly: false},
}

// rollupPrune is one table scheduled for deletion, with its window resolved.
type rollupPrune struct {
	table  string
	ageCol string
	days   int
}

// rollupPlan resolves the configured windows into the work to do.
//
// Split out from the deletion so the mapping from a window to the tables it
// governs can be tested without a database. Getting it wrong is silent until
// somebody notices charts that are missing: applying the hourly window to the
// daily history would quietly truncate the long-range record.
func rollupPlan(w RollupWindows) []rollupPrune {
	plan := make([]rollupPrune, 0, len(rollupTables))
	for _, t := range rollupTables {
		days := w.DailyDays
		if t.hourly {
			days = w.HourlyDays
		}
		if days <= 0 {
			continue // 0 means keep forever
		}
		plan = append(plan, rollupPrune{table: t.name, ageCol: t.ageCol, days: days})
	}
	return plan
}

// runRollupRetention prunes every pre-aggregated table whose window is set.
//
// A failure on one table does not stop the others: they grow independently,
// and one bad table should not exempt the rest from being bounded.
func runRollupRetention(db *gorm.DB, w RollupWindows) {
	if db == nil {
		return
	}
	for _, p := range rollupPlan(w) {
		deleted, err := pruneRollupTable(db, p)
		if err != nil {
			log.Printf("rollup retention: %s: %v", p.table, err)
			continue
		}
		if deleted > 0 {
			log.Printf("rollup retention: deleted %d row(s) older than %d days from %s",
				deleted, p.days, p.table)
		}
	}
}

// pruneRollupTable deletes rows older than the window, in bounded batches.
//
// Batched, and keyed on `id`, for the same reasons the events pass is: one
// statement across a table this size would hold a single transaction and blow
// up WAL, and `ctid` is not unique across a hypertable's chunks.
//
// Deleting does not return space to the operating system. It marks pages
// reusable, so the relation stops growing and autovacuum recycles the space for
// new rows, but the file a large table already occupies stays allocated.
// Reclaiming that needs VACUUM FULL (or a table rewrite) under an exclusive
// lock — an operator decision, not something a background worker should make.
func pruneRollupTable(db *gorm.DB, p rollupPrune) (int64, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(p.days) * 24 * time.Hour)

	// The table and column names are interpolated because they cannot be bind
	// parameters — and they are safe to interpolate because they come from the
	// package-level rollupTables list above and never from a caller.
	query := fmt.Sprintf(`
		DELETE FROM %s
		WHERE id IN (
			SELECT id FROM %s
			WHERE %s < ?
			LIMIT ?
		)
	`, p.table, p.table, p.ageCol)

	total := int64(0)
	for {
		result := db.Exec(query, cutoff, retentionBatchSize)
		if result.Error != nil {
			return total, result.Error
		}
		if result.RowsAffected == 0 {
			break
		}
		total += result.RowsAffected
	}

	if total > 0 {
		// Same reason as the events pass: a stale row count is what makes the
		// dashboard's range queries choose a sequential scan.
		if err := db.Exec("ANALYZE " + p.table).Error; err != nil {
			log.Printf("rollup retention: warning: could not ANALYZE %s: %v", p.table, err)
		}
	}

	return total, nil
}
