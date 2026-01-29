package db

import (
	"log"
	"sort"
	"time"

	"gorm.io/gorm"
)

// runAggregationOnce aggregates events for the given hour (bucketStart to bucketStart+1h)
// into MetricBucket rows. Call with bucketStart = time in UTC truncated to hour.
func runAggregationOnce(db *gorm.DB, bucketStart time.Time) error {
	bucketEnd := bucketStart.Add(time.Hour)

	var events []Event
	if err := db.Where("created_at >= ? AND created_at < ?", bucketStart, bucketEnd).
		Select("user_id", "project", "status", "duration_ms").
		Find(&events).Error; err != nil {
		return err
	}

	// Group by (user_id, project); collect status and duration_ms for percentiles.
	type key struct {
		UserID  string
		Project string
	}
	groups := make(map[key][]struct {
		status int
		dur    int64
	})
	for _, e := range events {
		k := key{UserID: e.UserID, Project: e.Project}
		groups[k] = append(groups[k], struct {
			status int
			dur    int64
		}{e.Status, e.DurationMs})
	}

	for k, list := range groups {
		total := int64(len(list))
		var errorCount int64
		durations := make([]int64, 0, len(list))
		for _, p := range list {
			if p.status >= 400 {
				errorCount++
			}
			durations = append(durations, p.dur)
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p50 := int64(0)
		p95 := int64(0)
		p99 := int64(0)
		if n := len(durations); n > 0 {
			p50 = durations[(n*50)/100]
			p95 = durations[(n*95)/100]
			p99 = durations[(n*99)/100]
		}

		row := MetricBucket{
			UserID:        k.UserID,
			Project:       k.Project,
			BucketStart:   bucketStart,
			TotalCount:    total,
			ErrorCount:    errorCount,
			DurationP50Ms: p50,
			DurationP95Ms: p95,
			DurationP99Ms: p99,
		}
		var existing MetricBucket
		err := db.Where("user_id = ? AND project = ? AND bucket_start = ?", k.UserID, k.Project, bucketStart).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			err = db.Create(&row).Error
		} else if err == nil {
			err = db.Model(&existing).Updates(map[string]interface{}{
				"total_count":     row.TotalCount,
				"error_count":     row.ErrorCount,
				"duration_p50_ms": row.DurationP50Ms,
				"duration_p95_ms": row.DurationP95Ms,
				"duration_p99_ms": row.DurationP99Ms,
			}).Error
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// StartAggregationWorker runs aggregation for the previous full hour at startup,
// then every hour. Buckets are in UTC.
func StartAggregationWorker(db *gorm.DB) {
	go func() {
		// Run for the last 24 completed hours at startup.
		now := time.Now().UTC()
		for i := 1; i <= 24; i++ {
			bucketStart := now.Truncate(time.Hour).Add(-time.Duration(i) * time.Hour)
			if err := runAggregationOnce(db, bucketStart); err != nil {
				log.Printf("aggregation error (startup) for %s: %v", bucketStart.Format(time.RFC3339), err)
			}
		}

		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for t := range ticker.C {
			bucketStart := t.UTC().Truncate(time.Hour).Add(-time.Hour)
			if err := runAggregationOnce(db, bucketStart); err != nil {
				log.Printf("aggregation error for %s: %v", bucketStart.Format(time.RFC3339), err)
			}
		}
	}()
}
