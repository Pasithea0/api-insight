package db

import (
	"log"
	"time"

	"gorm.io/gorm"
)

// runRetentionOnce performs a single pass of retention cleanup,
// deleting any events whose ExpiresAt is in the past.
func runRetentionOnce(db *gorm.DB) error {
	now := time.Now()
	if err := db.Where("expires_at IS NOT NULL AND expires_at <= ?", now).Delete(&Event{}).Error; err != nil {
		return err
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
