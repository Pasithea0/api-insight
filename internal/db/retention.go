package db

import (
	"log"
	"time"

	"gorm.io/gorm"
)

// runRetentionOnce performs a single pass of retention cleanup,
// deleting any events whose ExpiresAt is in the past in batches.
func runRetentionOnce(db *gorm.DB) error {
	now := time.Now()
	batchSize := 50000

	for {
		result := db.Exec(`
			DELETE FROM events
			WHERE ctid IN (
				SELECT ctid FROM events
				WHERE expires_at IS NOT NULL AND expires_at <= ?
				LIMIT ?
			)
		`, now, batchSize)

		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected == 0 {
			break
		}
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
