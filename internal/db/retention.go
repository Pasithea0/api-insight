package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
)

// retentionBatchSize bounds how many rows a single delete statement removes,
// so a large backlog cannot hold one long transaction and blow up WAL.
const retentionBatchSize = 50000

// runRetentionOnce performs a single pass of per-API-key retention cleanup,
// deleting events whose ExpiresAt has passed, in batches.
//
// The delete is keyed on `id`, NOT on `ctid`. That distinction is not
// cosmetic: `ctid` is a physical row address that is only unique within a
// single table, and on a hypertable every chunk is a separate table. The
// previous `WHERE ctid IN (SELECT ctid FROM events ...)` form therefore
// matched the *same* ctid in every chunk, deleting rows that were not
// expired. Verified on TimescaleDB 2.30: with 10 expired rows in one chunk
// and 10 live rows in another, the ctid form removed 10 rows — 5 of them
// live — while the id form removed exactly the 5 expired rows. Only `id`
// is unique across the whole hypertable.
//
// This runs on plain Postgres too, where it behaves identically.
func runRetentionOnce(db *gorm.DB) error {
	now := time.Now().UTC()
	total := int64(0)

	for {
		result := db.Exec(`
			DELETE FROM events
			WHERE id IN (
				SELECT id FROM events
				WHERE expires_at IS NOT NULL AND expires_at <= ?
				LIMIT ?
			)
		`, now, retentionBatchSize)

		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			break
		}
		total += result.RowsAffected
	}

	if total > 0 {
		log.Printf("retention: deleted %d expired events", total)
	}

	// Refresh table statistics after mass deletes so the query planner
	// doesn't use stale row counts.
	if err := db.Exec("ANALYZE events").Error; err != nil {
		log.Printf("retention: warning: could not ANALYZE events after cleanup: %v", err)
	}

	return nil
}

// StartRetentionWorker launches a background goroutine that runs the
// retention cleanup once at startup and then once per day.
//
// On TimescaleDB this complements the chunk-drop retention policy rather
// than duplicating it: the policy enforces a global age ceiling by dropping
// whole chunks, while this worker honours the shorter, per-API-key expiry
// windows that individual keys are allowed to request.
//
// objects selects the driver. When the events live in an object store there
// are no rows to delete and the SQL pass is skipped entirely — instead the
// hour buckets that no project could still want are dropped, where "no
// project" means the longest window any key asks for. Shorter per-project
// windows do not need this pass at all: the reader applies them as it reads,
// which is the only way to honour a window on data that cannot be rewritten.
//
// The pre-aggregated tables are pruned by the same worker but under their own
// windows, and that pass runs whatever the event store is: they are always in
// Postgres, and skipping them in object-store mode is exactly how 185 GB of
// rollups accumulated behind an events table of 70 GB.
func StartRetentionWorker(db *gorm.DB, objects ObjectRetention, windows ProjectRetentionLookup, rollups RollupWindows) {
	go func() {
		run := func() {
			switch {
			case objects != nil:
				runObjectRetention(objects, windows)
			case windows != nil && windows.MaxRetentionDays() <= 0:
				// Unlimited retention with the events in Postgres. There is
				// nothing to delete, and running the pass would be a full
				// scan that removes nothing. Rows ingested while retention
				// was still limited keep whatever expiry they were stamped
				// with, though, so this only stops *new* data from expiring.
				log.Printf("retention: unlimited (APP_RETENTION_DAYS=0) — rows already stamped with an expiry still expire; only new events are kept forever")
			default:
				if err := runRetentionOnce(db); err != nil {
					log.Printf("retention cleanup error: %v", err)
				}
			}

			runRollupRetention(db, rollups)
		}

		run()

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			run()
		}
	}()
}

// WarnOrphanedEventsTable logs a one-off hint when the Postgres events table
// still holds rows but is no longer being written to.
//
// After a switch to an object store the table is dead weight: nothing writes
// it, nothing prunes it (retention follows the configured store), and its
// pages stay allocated on the host whose disk is the reason for the switch in
// the first place. The estimate comes from the planner's statistics, so this
// costs one catalog read rather than a count over millions of rows.
//
// It deliberately does NOT clean up. Dropping data is the operator's call,
// and on a TimescaleDB hypertable the obvious statement is the wrong one:
// `TRUNCATE ONLY events` fails outright, and `VACUUM FULL events` on the
// parent table reclaims nothing from the chunks.
func WarnOrphanedEventsTable(db *gorm.DB) {
	if db == nil {
		return
	}
	var estimate float64
	if err := db.Raw(`SELECT COALESCE(reltuples, 0) FROM pg_class WHERE oid = 'events'::regclass`).
		Scan(&estimate).Error; err != nil {
		// The table may not exist at all, which is the ideal state.
		return
	}
	if estimate < 1000 {
		return
	}

	var isHyper bool
	_ = db.Raw(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&isHyper).Error

	log.Printf("event store: the Postgres events table still holds about %.0f rows but is no longer written to (APP_EVENT_STORE=s3). "+
		"They are dead weight on this host's disk. Reclaiming them is a deliberate, destructive step:", estimate)
	if isHyper {
		log.Printf("  this looks like a TimescaleDB hypertable, so use the chunk-aware form:")
		log.Printf("    SELECT show_chunks('events');                    -- inspect first")
		log.Printf("    SELECT drop_chunks('events');                    -- drops every chunk; frees disk")
		log.Printf("    -- or, equivalently and simpler: TRUNCATE events;  (NOT \"TRUNCATE ONLY events\", which fails)")
	} else {
		log.Printf("    TRUNCATE events;   -- releases the space to the OS immediately")
	}
}

// ObjectRetention is the object store's half of retention: it can drop whole
// hour buckets, nothing finer.
type ObjectRetention interface {
	// DeleteBefore removes objects whose entire hour bucket is older than
	// cutoff and reports how many were removed.
	DeleteBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// runObjectRetention drops the hour buckets no project could still want.
func runObjectRetention(objects ObjectRetention, windows ProjectRetentionLookup) {
	days := 0
	if windows != nil {
		days = windows.MaxRetentionDays()
	}
	if days <= 0 {
		log.Printf("retention: no retention window is configured for the object store; skipping object deletion")
		return
	}

	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	removed, err := objects.DeleteBefore(ctx, cutoff)
	if err != nil {
		log.Printf("retention: object store cleanup before %s failed after removing %d object(s): %v",
			cutoff.Format(time.RFC3339), removed, err)
		return
	}
	if removed > 0 {
		log.Printf("retention: deleted %d object(s) older than %s from the object store (longest window: %d days)",
			removed, cutoff.Format(time.RFC3339), days)
	}
}

// ProjectRetentionResult reports what one retention edit actually changed.
type ProjectRetentionResult struct {
	// Deleted counts events removed because they were already older than
	// the new window.
	Deleted int64
	// Rewritten counts surviving events whose expires_at was moved onto
	// the new window.
	Rewritten int64
}

// ApplyProjectRetention reconciles the stored events of one (user, project)
// with a new retention window and reports what it changed.
//
// Ingest stamps every row with `expires_at = created_at + <window in effect
// when the row arrived>` (see IngestHandler), so editing a project's retention
// cannot be just an UPDATE of the API key: rows written under the old window
// keep the old expiry. Shrinking 90 → 7 days would then leave week-old data in
// place until its original 90-day expiry, and growing 7 → 90 would still
// expire rows a week after they were written — both silently contradicting the
// setting the operator is looking at. So an edit does two things:
//
//  1. rows already older than the new window are deleted now, in batches,
//     instead of waiting for the next daily worker pass;
//  2. every surviving row's expires_at is rewritten to created_at + window, so
//     a shrink expires them on the new schedule and a grow keeps them.
//
// Deletion follows created_at rather than expires_at so it agrees with the
// TimescaleDB chunk-drop policy, which also caps raw data by age. Both steps
// are batched and keyed on `id`, never on `ctid` (see runRetentionOnce for the
// hypertable reason).
//
// retentionDays must be positive. 0 is the API key model's "use the configured
// default" sentinel, so callers resolve that to a concrete window first and a
// non-positive value here is a programming error rather than a policy.
func ApplyProjectRetention(db *gorm.DB, userID, project string, retentionDays int) (ProjectRetentionResult, error) {
	var res ProjectRetentionResult
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(project) == "" {
		return res, errors.New("retention: userID and project are required")
	}
	if retentionDays <= 0 {
		return res, fmt.Errorf("retention: retention days must be positive, got %d", retentionDays)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	deleted, err := deleteProjectEventsBefore(db, userID, project, cutoff)
	res.Deleted = deleted
	if err != nil {
		return res, fmt.Errorf("retention: delete events of %s/%s older than %s: %w",
			userID, project, cutoff.Format(time.RFC3339), err)
	}

	rewritten, err := rewriteProjectExpiry(db, userID, project, retentionDays)
	res.Rewritten = rewritten
	if err != nil {
		return res, fmt.Errorf("retention: rewrite expires_at for %s/%s: %w", userID, project, err)
	}

	// Refresh planner statistics after a mass delete: a stale row count is
	// what makes the dashboard's range queries pick a sequential scan.
	if res.Deleted > 0 {
		if err := db.Exec("ANALYZE events").Error; err != nil {
			log.Printf("retention: warning: could not ANALYZE events after project cleanup: %v", err)
		}
	}

	return res, nil
}

// deleteProjectEventsBefore removes a project's events older than cutoff, in
// bounded batches so one edit cannot hold a single long transaction and blow
// up WAL.
func deleteProjectEventsBefore(db *gorm.DB, userID, project string, cutoff time.Time) (int64, error) {
	total := int64(0)
	for {
		result := db.Exec(`
			DELETE FROM events
			WHERE id IN (
				SELECT id FROM events
				WHERE user_id = ? AND project = ? AND created_at < ?
				LIMIT ?
			)
		`, userID, project, cutoff, retentionBatchSize)

		if result.Error != nil {
			return total, result.Error
		}
		if result.RowsAffected == 0 {
			return total, nil
		}
		total += result.RowsAffected
	}
}

// rewriteProjectExpiry moves every surviving row of a project onto the new
// retention window.
//
// `expires_at IS DISTINCT FROM created_at + make_interval(days => n)` is what
// makes the pass bounded and idempotent: each batch clears the rows that still
// disagree with the window, so the loop ends when the project is consistent
// (including rows whose expires_at was NULL, which IS DISTINCT FROM any
// timestamp and so are given an expiry too — a project with a window must
// expire its rows, whatever they were stamped with before).
//
// make_interval() is used rather than `?::interval` so the day count is a
// plain integer bind parameter, not a string that has to survive Postgres
// interval parsing.
func rewriteProjectExpiry(db *gorm.DB, userID, project string, retentionDays int) (int64, error) {
	total := int64(0)
	for {
		result := db.Exec(`
			UPDATE events
			SET expires_at = created_at + make_interval(days => ?)
			WHERE id IN (
				SELECT id FROM events
				WHERE user_id = ? AND project = ?
				  AND expires_at IS DISTINCT FROM created_at + make_interval(days => ?)
				LIMIT ?
			)
		`, retentionDays, userID, project, retentionDays, retentionBatchSize)

		if result.Error != nil {
			return total, result.Error
		}
		if result.RowsAffected == 0 {
			return total, nil
		}
		total += result.RowsAffected
	}
}
