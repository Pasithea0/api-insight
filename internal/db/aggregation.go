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

	// 4. Upsert daily buckets after the day is complete (bucket lands on a new UTC day boundary)
	if bucketStart.Hour() == 23 {
		dayStart := bucketStart.Truncate(24 * time.Hour)
		if err := aggregateDailyMetrics(db, dayStart); err != nil {
			return err
		}
		if err := aggregateDailyRoutes(db, dayStart); err != nil {
			return err
		}
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
		backfillDailyBuckets(db)

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

// aggregateDailyMetrics rolls up all MetricBucket rows for a given UTC day
// into a single DailyMetricBucket row, using an upsert pattern.
func aggregateDailyMetrics(db *gorm.DB, dayStart time.Time) error {
	dayEnd := dayStart.Add(24 * time.Hour)

	type dailyRow struct {
		UserID      string
		Project     string
		TotalCount  int64
		ErrorCount  int64
		// Weighted average of percentiles, weighted by total_count
		P50Sum float64
		P95Sum float64
		P99Sum float64
		Count  int64
	}

	var rows []dailyRow
	if err := db.Raw(`
		SELECT
			user_id, project,
			SUM(total_count) AS total_count,
			SUM(error_count) AS error_count,
			SUM(duration_p50_ms * total_count) AS p50_sum,
			SUM(duration_p95_ms * total_count) AS p95_sum,
			SUM(duration_p99_ms * total_count) AS p99_sum,
			SUM(total_count) AS count
		FROM metric_buckets
		WHERE bucket_start >= ? AND bucket_start < ?
		GROUP BY user_id, project
	`, dayStart, dayEnd).Scan(&rows).Error; err != nil {
		return err
	}

	for _, r := range rows {
		var avgP50, avgP95, avgP99 int64
		if r.Count > 0 {
			avgP50 = int64(r.P50Sum / float64(r.Count))
			avgP95 = int64(r.P95Sum / float64(r.Count))
			avgP99 = int64(r.P99Sum / float64(r.Count))
		}

		row := DailyMetricBucket{
			UserID:        r.UserID,
			Project:       r.Project,
			BucketDate:    dayStart,
			TotalCount:    r.TotalCount,
			ErrorCount:    r.ErrorCount,
			DurationP50Ms: avgP50,
			DurationP95Ms: avgP95,
			DurationP99Ms: avgP99,
		}

		var existing DailyMetricBucket
		err := db.Where("user_id = ? AND project = ? AND bucket_date = ?", r.UserID, r.Project, dayStart).First(&existing).Error
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

// aggregateDailyRoutes rolls up all RouteBucket rows for a given UTC day
// into a single DailyRouteBucket row per (user, project, route).
func aggregateDailyRoutes(db *gorm.DB, dayStart time.Time) error {
	dayEnd := dayStart.Add(24 * time.Hour)

	type dailyRow struct {
		UserID      string
		Project     string
		Route       string
		TotalCount  int64
		ErrorCount  int64
		DurSum      float64
		Count       int64
		StatusJSON  string
	}

	var rows []dailyRow
	if err := db.Raw(`
		SELECT
			user_id, project, route,
			SUM(total_count) AS total_count,
			SUM(error_count) AS error_count,
			SUM(avg_duration_ms * total_count) AS dur_sum,
			SUM(total_count) AS count
		FROM route_buckets
		WHERE bucket_start >= ? AND bucket_start < ?
		GROUP BY user_id, project, route
	`, dayStart, dayEnd).Scan(&rows).Error; err != nil {
		return err
	}

	for _, r := range rows {
		avgDur := 0.0
		if r.Count > 0 {
			avgDur = r.DurSum / float64(r.Count)
		}

		// Compute status counts directly from events for the day
		// (more reliable than merging hourly JSON blobs)
		type scRow struct {
			Status int
			Count  int64
		}
		var daySCs []scRow
		if err := db.Model(&Event{}).
			Select("status, COUNT(*) AS count").
			Where("user_id = ? AND project = ? AND route = ? AND created_at >= ? AND created_at < ?",
				r.UserID, r.Project, r.Route, dayStart, dayEnd).
			Group("status").
			Scan(&daySCs).Error; err != nil {
			return err
		}
		statusJSON := make(map[string]interface{}, len(daySCs))
		for _, sc := range daySCs {
			statusJSON[fmt.Sprint(sc.Status)] = sc.Count
		}

		row := DailyRouteBucket{
			UserID:        r.UserID,
			Project:       r.Project,
			Route:         r.Route,
			BucketDate:    dayStart,
			TotalCount:    r.TotalCount,
			ErrorCount:    r.ErrorCount,
			AvgDurationMs: avgDur,
			StatusCounts:  statusJSON,
		}

		var existing DailyRouteBucket
		err := db.Where("user_id = ? AND project = ? AND route = ? AND bucket_date = ?",
			r.UserID, r.Project, r.Route, dayStart).First(&existing).Error
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

// backfillDailyBuckets computes DailyMetricBucket and DailyRouteBucket rows for
// all prior days that don't yet have them, by rolling up existing hourly buckets.
func backfillDailyBuckets(db *gorm.DB) {
	now := time.Now().UTC()
	// Find the earliest hour bucket that exists
	var earliest MetricBucket
	if err := db.Order("bucket_start ASC").Limit(1).Find(&earliest).Error; err != nil || earliest.ID == 0 {
		log.Println("backfill daily: no hourly buckets found (nothing to backfill)")
		return
	}

	dayStart := earliest.BucketStart.Truncate(24 * time.Hour)
	todayStart := now.Truncate(24 * time.Hour)
	count := 0

	for dayStart.Before(todayStart) {
		var existing DailyMetricBucket
		err := db.Where("bucket_date = ?", dayStart).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			// Check if we have complete data for this day (all 24 hours)
			var hourCount int64
			db.Model(&MetricBucket{}).Where("bucket_start >= ? AND bucket_start < ?",
				dayStart, dayStart.Add(24*time.Hour)).Distinct("bucket_start").Count(&hourCount)
			if hourCount > 0 {
				if err := aggregateDailyMetrics(db, dayStart); err != nil {
					log.Printf("backfill daily metrics for %s: %v", dayStart.Format("2006-01-02"), err)
				}
				if err := aggregateDailyRoutes(db, dayStart); err != nil {
					log.Printf("backfill daily routes for %s: %v", dayStart.Format("2006-01-02"), err)
				}
				count++
			}
		}
		dayStart = dayStart.Add(24 * time.Hour)
	}
	if count > 0 {
		log.Printf("backfill daily: populated %d missing day(s)", count)
	} else {
		log.Println("backfill daily: all days up to date")
	}
}
