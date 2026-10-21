package db

import (
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
func StartRetentionWorker(db *gorm.DB) {
	go func() {
		if err := runRetentionOnce(db); err != nil {
			log.Printf("retention cleanup error (startup): %v", err)
		}

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			if err := runRetentionOnce(db); err != nil {
				log.Printf("retention cleanup error: %v", err)
			}
		}
	}()
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
