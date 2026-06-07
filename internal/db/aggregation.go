package db

import (
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
)

// runAggregationOnce aggregates events for the given hour (bucketStart to bucketStart+1h)
// into MetricBucket, RouteBucket, and AttributeKeyIndex rows.
// Call with bucketStart = time in UTC truncated to hour.
func runAggregationOnce(db *gorm.DB, bucketStart time.Time) error {
	bucketEnd := bucketStart.Add(time.Hour)

	// 1. Aggregate overall metrics into MetricBucket
	if err := aggregateMetricBuckets(db, bucketStart, bucketEnd); err != nil {
		return err
	}

	// 2. Aggregate route-level stats into RouteBucket
	if err := aggregateRouteBuckets(db, bucketStart, bucketEnd); err != nil {
		return err
	}

	// 3. Extract attribute keys into AttributeKeyIndex
	if err := aggregateAttributeKeys(db, bucketStart, bucketEnd); err != nil {
		return err
	}

	return nil
}

type metricAggRow struct {
	UserID     string
	Project    string
	TotalCount int64
	ErrorCount int64
	P50        float64
	P95        float64
	P99        float64
}

func aggregateMetricBuckets(db *gorm.DB, bucketStart, bucketEnd time.Time) error {
	var rows []metricAggRow
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

type routeAggRow struct {
	UserID      string
	Project     string
	Route       string
	Status      int
	Count       int64
	AvgDuration float64
}

func aggregateRouteBuckets(db *gorm.DB, bucketStart, bucketEnd time.Time) error {
	var statusRows []routeAggRow
	err := db.Raw(`
		SELECT user_id, project, route, status, COUNT(*) as count, AVG(duration_ms) as avg_duration
		FROM events
		WHERE created_at >= ? AND created_at < ?
		GROUP BY user_id, project, route, status
	`, bucketStart, bucketEnd).Scan(&statusRows).Error
	if err != nil {
		return err
	}

	type routeKey struct {
		UserID  string
		Project string
		Route   string
	}
	type routeAccum struct {
		TotalCount int64
		ErrorCount int64
		DurSum     float64
		StatusCnt  map[string]int64
	}
	merged := make(map[routeKey]*routeAccum)
	for _, r := range statusRows {
		key := routeKey{UserID: r.UserID, Project: r.Project, Route: r.Route}
		a, ok := merged[key]
		if !ok {
			a = &routeAccum{StatusCnt: make(map[string]int64)}
			merged[key] = a
		}
		a.TotalCount += r.Count
		if r.Status >= 400 {
			a.ErrorCount += r.Count
		}
		a.DurSum += r.AvgDuration * float64(r.Count)
		a.StatusCnt[fmt.Sprint(r.Status)] = r.Count
	}

	for key, a := range merged {
		avgDur := 0.0
		if a.TotalCount > 0 {
			avgDur = a.DurSum / float64(a.TotalCount)
		}
		statusJSON := make(map[string]interface{}, len(a.StatusCnt))
		for k, v := range a.StatusCnt {
			statusJSON[k] = v
		}

		row := RouteBucket{
			UserID:        key.UserID,
			Project:       key.Project,
			Route:         key.Route,
			BucketStart:   bucketStart,
			TotalCount:    a.TotalCount,
			ErrorCount:    a.ErrorCount,
			AvgDurationMs: avgDur,
			StatusCounts:  statusJSON,
		}

		var existing RouteBucket
		err := db.Where("user_id = ? AND project = ? AND route = ? AND bucket_start = ?",
			key.UserID, key.Project, key.Route, bucketStart).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			err = db.Create(&row).Error
		} else if err == nil {
			err = db.Model(&existing).Updates(map[string]interface{}{
				"total_count":     row.TotalCount,
				"error_count":     row.ErrorCount,
				"avg_duration_ms": row.AvgDurationMs,
				"status_counts":   row.StatusCounts,
			}).Error
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func aggregateAttributeKeys(db *gorm.DB, bucketStart, bucketEnd time.Time) error {
	type attrKeyRow struct {
		UserID  string
		Project string
		Key     string
	}
	var rows []attrKeyRow
	err := db.Raw(`
		SELECT DISTINCT user_id, project, jsonb_object_keys(attributes) AS key
		FROM events
		WHERE created_at >= ? AND created_at < ? AND attributes IS NOT NULL AND attributes != '{}'::jsonb
	`, bucketStart, bucketEnd).Scan(&rows).Error
	if err != nil {
		return err
	}

	for _, r := range rows {
		var existing AttributeKeyIndex
		err := db.Where("user_id = ? AND project = ? AND key = ?", r.UserID, r.Project, r.Key).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			err = db.Create(&AttributeKeyIndex{
				UserID:  r.UserID,
				Project: r.Project,
				Key:     r.Key,
			}).Error
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// backfillRouteBucketStatuses fills in NULL status_counts for existing route_buckets.
// This is a one-time migration for rows created before the StatusCounts column was added.
func backfillRouteBucketStatuses(db *gorm.DB) {
	var nullRows []struct {
		ID          uint
		UserID      string
		Project     string
		Route       string
		BucketStart time.Time
	}
	if err := db.Model(&RouteBucket{}).
		Where("status_counts IS NULL").
		Find(&nullRows).Error; err != nil {
		log.Printf("backfill: could not find null status_counts: %v", err)
		return
	}
	if len(nullRows) == 0 {
		return
	}
	log.Printf("backfill: filling status_counts for %d route_buckets...", len(nullRows))
	for _, r := range nullRows {
		type scRow struct {
			Status int
			Count  int64
		}
		var scs []scRow
		if err := db.Model(&Event{}).
			Select("status, COUNT(*) as count").
			Where("user_id = ? AND project = ? AND route = ? AND created_at >= ? AND created_at < ?",
				r.UserID, r.Project, r.Route, r.BucketStart, r.BucketStart.Add(time.Hour)).
			Group("status").
			Scan(&scs).Error; err != nil {
			log.Printf("backfill: error querying for %s/%s/%s: %v", r.UserID, r.Project, r.Route, err)
			continue
		}
		if len(scs) == 0 {
			continue
		}
		m := make(map[string]interface{}, len(scs))
		for _, s := range scs {
			m[fmt.Sprint(s.Status)] = s.Count
		}
		if err := db.Model(&RouteBucket{}).Where("id = ?", r.ID).
			Update("status_counts", m).Error; err != nil {
			log.Printf("backfill: update error for id %d: %v", r.ID, err)
		}
	}
	log.Println("backfill: done")
}

// StartAggregationWorker runs aggregation for the previous full hour at startup,
// then every hour. Buckets are in UTC.
func StartAggregationWorker(db *gorm.DB) {
	go func() {
		backfillRouteBucketStatuses(db)

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
