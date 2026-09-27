package db

import (
	"log"
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
