package db

import (
	"log"
	"time"

	"gorm.io/gorm"
)

// runAggregationOnce aggregates events for the given hour (bucketStart to bucketStart+1h)
// into MetricBucket rows. Call with bucketStart = time in UTC truncated to hour.
func runAggregationOnce(db *gorm.DB, bucketStart time.Time) error {
	bucketEnd := bucketStart.Add(time.Hour)

	type aggRow struct {
		UserID     string
		Project    string
		TotalCount int64
		ErrorCount int64
		P50        float64
		P95        float64
		P99        float64
	}

	var rows []aggRow
	err := db.Raw(`
		SELECT 
			user_id, 
			project, 
			COUNT(*) as total_count, 
			COUNT(*) FILTER (WHERE status >= 400) as error_count,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) as p50,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) as p95,
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) as p99
		FROM events 
		WHERE created_at >= ? AND created_at < ?
		GROUP BY user_id, project
	`, bucketStart, bucketEnd).Scan(&rows).Error
	if err != nil {
		return err
	}

	for _, r := range rows {
		row := MetricBucket{
			UserID:        r.UserID,
			Project:       r.Project,
			BucketStart:   bucketStart,
			TotalCount:    r.TotalCount,
			ErrorCount:    r.ErrorCount,
			DurationP50Ms: int64(r.P50),
			DurationP95Ms: int64(r.P95),
			DurationP99Ms: int64(r.P99),
		}

		var existing MetricBucket
		err := db.Where("user_id = ? AND project = ? AND bucket_start = ?", r.UserID, r.Project, bucketStart).First(&existing).Error
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
