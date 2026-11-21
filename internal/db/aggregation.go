package db

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"gorm.io/gorm"
)

// runAggregation builds the hourly rollups for one bucket, using whichever
// source the deployment is configured for.
//
// Two drivers exist because the two stores answer different questions. A SQL
// store can GROUP BY, so it aggregates in the database and the worker only
// moves a few hundred rows. An object store cannot, so the same numbers are
// produced by streaming the hour back and accumulating in Go. Both write the
// identical bucket rows, so everything downstream — every chart, the daily
// rollups, the attribute index — is unaware of which ran.
func runAggregation(db *gorm.DB, scanner EventScanner, bucketStart time.Time) error {
	if scanner != nil {
		return runAggregationFromScanner(db, bucketStart.UTC(), scanner)
	}
	return runAggregationOnce(db, bucketStart.UTC())
}

// runAggregationOnce aggregates events for the given hour (bucketStart to
// bucketStart+1h) into MetricBucket, RouteBucket, and AttributeKeyIndex rows.
// Call with bucketStart = time in UTC truncated to hour.
func runAggregationOnce(db *gorm.DB, bucketStart time.Time) error {
	bucketEnd := bucketStart.Add(time.Hour)

	// When TimescaleDB continuous aggregates are in play, materialise the
	// bucket we are about to roll up. Doing it here (rather than trusting
	// the background refresh schedule) keeps the worker's output
	// deterministic: it never reads a half-materialised bucket.
	if err := refreshRollupAggregates(db, bucketStart, bucketEnd); err != nil {
		return err
	}

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
		if err := aggregateDailyRoutes(db, dayStart, false); err != nil {
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

// upsertMetricBucket writes one hourly metric rollup, creating it or
// replacing the numbers in place. Both drivers funnel through here so the
// bucket a chart reads cannot depend on which store produced it.
func upsertMetricBucket(db *gorm.DB, row MetricBucket) error {
	var existing MetricBucket
	err := db.Where("user_id = ? AND project = ? AND bucket_start = ?",
		row.UserID, row.Project, row.BucketStart).First(&existing).Error
	switch {
	case err == gorm.ErrRecordNotFound:
		return db.Create(&row).Error
	case err == nil:
		return db.Model(&existing).Updates(map[string]interface{}{
			"total_count":     row.TotalCount,
			"error_count":     row.ErrorCount,
			"duration_p50_ms": row.DurationP50Ms,
			"duration_p95_ms": row.DurationP95Ms,
			"duration_p99_ms": row.DurationP99Ms,
		}).Error
	default:
		return err
	}
}

// upsertRouteBucket writes one hourly route rollup.
func upsertRouteBucket(db *gorm.DB, row RouteBucket) error {
	var existing RouteBucket
	err := db.Where("user_id = ? AND project = ? AND route = ? AND bucket_start = ?",
		row.UserID, row.Project, row.Route, row.BucketStart).First(&existing).Error
	switch {
	case err == gorm.ErrRecordNotFound:
		return db.Create(&row).Error
	case err == nil:
		return db.Model(&existing).Updates(map[string]interface{}{
			"total_count":     row.TotalCount,
			"error_count":     row.ErrorCount,
			"avg_duration_ms": row.AvgDurationMs,
			"status_counts":   row.StatusCounts,
		}).Error
	default:
		return err
	}
}

// upsertAttributeKey records that a project uses an attribute key. It is
// idempotent: the index is a set, and re-seeing a key is the normal case for
// every hourly pass that follows the first.
func upsertAttributeKey(db *gorm.DB, userID, project, key string) error {
	var existing AttributeKeyIndex
	err := db.Where("user_id = ? AND project = ? AND key = ?", userID, project, key).First(&existing).Error
	switch {
	case err == gorm.ErrRecordNotFound:
		return db.Create(&AttributeKeyIndex{UserID: userID, Project: project, Key: key}).Error
	case err == nil:
		return nil
	default:
		return err
	}
}

func aggregateMetricBuckets(db *gorm.DB, bucketStart, bucketEnd time.Time) error {
	var rows []metricAggRow
	// Source relation: the TimescaleDB continuous aggregate is pre-computed per
	// hour, so this reads a few hundred rows instead of scanning every raw
	// event in the bucket. The plain-events query is the fallback.
	query := `
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
	`
	if UseContinuousAggregates() {
		query = `
			SELECT user_id, project, total_count, error_count, p50, p95, p99
			FROM ` + continuousAggregateMetrics + `
			WHERE bucket_start >= ? AND bucket_start < ?
		`
	}
	err := db.Raw(query, bucketStart, bucketEnd).Scan(&rows).Error
	if err != nil {
		return err
	}

	for _, r := range rows {
		if err := upsertMetricBucket(db, MetricBucket{
			UserID:        r.UserID,
			Project:       r.Project,
			BucketStart:   bucketStart,
			TotalCount:    r.TotalCount,
			ErrorCount:    r.ErrorCount,
			DurationP50Ms: int64(r.P50),
			DurationP95Ms: int64(r.P95),
			DurationP99Ms: int64(r.P99),
		}); err != nil {
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
	query := `
		SELECT user_id, project, route, status, COUNT(*) as count, AVG(duration_ms) as avg_duration
		FROM events
		WHERE created_at >= ? AND created_at < ?
		GROUP BY user_id, project, route, status
	`
	if UseContinuousAggregates() {
		query = `
			SELECT user_id, project, route, status, count, avg_duration
			FROM ` + continuousAggregateRoutes + `
			WHERE bucket_start >= ? AND bucket_start < ?
		`
	}
	err := db.Raw(query, bucketStart, bucketEnd).Scan(&statusRows).Error
	if err != nil {
		return err
	}

	for key, a := range mergeRouteStatusRows(statusRows) {
		if err := upsertRouteBucket(db, key.bucket(a, bucketStart)); err != nil {
			return err
		}
	}
	return nil
}

type routeBucketKey struct {
	UserID  string
	Project string
	Route   string
}

type routeAccum struct {
	TotalCount int64
	ErrorCount int64
	DurSum     float64
	StatusCnt  map[int]int64
}

// bucket renders the accumulated per-status rows for one route as the
// hourly bucket row both drivers write.
func (k routeBucketKey) bucket(a *routeAccum, bucketStart time.Time) RouteBucket {
	avgDur := 0.0
	if a.TotalCount > 0 {
		avgDur = a.DurSum / float64(a.TotalCount)
	}
	statusJSON := make(map[string]interface{}, len(a.StatusCnt))
	for status, count := range a.StatusCnt {
		statusJSON[fmt.Sprint(status)] = count
	}
	return RouteBucket{
		UserID:        k.UserID,
		Project:       k.Project,
		Route:         k.Route,
		BucketStart:   bucketStart,
		TotalCount:    a.TotalCount,
		ErrorCount:    a.ErrorCount,
		AvgDurationMs: avgDur,
		StatusCounts:  statusJSON,
	}
}

// mergeRouteStatusRows folds a per-status result set into one accumulator per
// (user, project, route). The SQL driver produces the rows with GROUP BY; the
// scanner driver calls it with rows it built itself, so the merge — and
// therefore the arithmetic — is shared and cannot drift.
func mergeRouteStatusRows(rows []routeAggRow) map[routeBucketKey]*routeAccum {
	merged := make(map[routeBucketKey]*routeAccum, len(rows))
	for _, r := range rows {
		key := routeBucketKey{UserID: r.UserID, Project: r.Project, Route: r.Route}
		a, ok := merged[key]
		if !ok {
			a = &routeAccum{StatusCnt: make(map[int]int64)}
			merged[key] = a
		}
		a.TotalCount += r.Count
		if r.Status >= 400 {
			a.ErrorCount += r.Count
		}
		a.DurSum += r.AvgDuration * float64(r.Count)
		a.StatusCnt[r.Status] += r.Count
	}
	return merged
}

func aggregateAttributeKeys(db *gorm.DB, bucketStart, bucketEnd time.Time) error {
	type attrKeyRow struct {
		UserID  string
		Project string
		Key     string
	}
	var rows []attrKeyRow
	// Guard on jsonb_typeof rather than `attributes IS NOT NULL AND
	// attributes != '{}'`. A JSON `null` scalar is not SQL NULL, so it
	// passed the old guard and then jsonb_object_keys() raised
	// "cannot call jsonb_object_keys on a scalar", failing the whole
	// aggregation pass. Only a JSON object has keys.
	err := db.Raw(`
		SELECT DISTINCT user_id, project, jsonb_object_keys(attributes) AS key
		FROM events
		WHERE created_at >= ? AND created_at < ? AND jsonb_typeof(attributes) = 'object'
	`, bucketStart, bucketEnd).Scan(&rows).Error
	if err != nil {
		return err
	}

	for _, r := range rows {
		if err := upsertAttributeKey(db, r.UserID, r.Project, r.Key); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Object-store driver
// ---------------------------------------------------------------------------

// scanMetricAccum accumulates one (user, project)'s hour.
//
// Durations are kept in full rather than summarised because the three
// stored percentiles cannot be derived from a mean and a count. At ~540k
// events in a busy hour that is a few megabytes, which is the price of
// computing percentiles outside a database.
type scanMetricAccum struct {
	Total     int64
	Errors    int64
	Durations []int64
}

type scanRouteAccum struct {
	Total  int64
	Errors int64
	DurSum int64
	Status map[int]int64
}

// runAggregationFromScanner builds one hour's rollups by streaming that hour
// back out of the configured store (the object store) and aggregating in Go.
//
// The output is identical to runAggregationOnce's: same bucket rows, same
// daily rollup, so a deployment that switches stores keeps its charts.
func runAggregationFromScanner(db *gorm.DB, bucketStart time.Time, scanner EventScanner) error {
	bucketEnd := bucketStart.Add(time.Hour)

	metrics := map[string]*scanMetricAccum{}
	routes := map[string]*routeAccum{}
	routeKeys := map[string]routeBucketKey{}
	attrs := map[[3]string]struct{}{}

	// ScanEvents takes an inclusive end; the bucket is half-open. Subtracting
	// a nanosecond keeps events stamped exactly on the next hour out of this
	// bucket, where the next pass will pick them up.
	end := bucketEnd.Add(-time.Nanosecond)

	err := scanner.ScanEvents(context.Background(), bucketStart, end, ScanFilter{}, func(ev Event) error {
		metricKey := ev.UserID + "|" + ev.Project
		m, ok := metrics[metricKey]
		if !ok {
			m = &scanMetricAccum{}
			metrics[metricKey] = m
		}
		m.Total++
		if ev.Status >= 400 {
			m.Errors++
		}
		m.Durations = append(m.Durations, ev.DurationMs)

		rk := routeBucketKey{UserID: ev.UserID, Project: ev.Project, Route: ev.Route}
		rkey := rk.UserID + "|" + rk.Project + "|" + rk.Route
		a, ok := routes[rkey]
		if !ok {
			a = &routeAccum{StatusCnt: map[int]int64{}}
			routes[rkey] = a
			routeKeys[rkey] = rk
		}
		a.TotalCount++
		if ev.Status >= 400 {
			a.ErrorCount++
		}
		a.DurSum += float64(ev.DurationMs)
		a.StatusCnt[ev.Status]++

		for k := range ev.Attributes {
			attrs[[3]string{ev.UserID, ev.Project, k}] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("aggregation: scan %s: %w", bucketStart.Format(time.RFC3339), err)
	}

	for metricKey, m := range metrics {
		userID, project := splitMetricKey(metricKey, m)
		p50, p95, p99 := percentilesOf(m.Durations)
		if err := upsertMetricBucket(db, MetricBucket{
			UserID:        userID,
			Project:       project,
			BucketStart:   bucketStart,
			TotalCount:    m.Total,
			ErrorCount:    m.Errors,
			DurationP50Ms: p50,
			DurationP95Ms: p95,
			DurationP99Ms: p99,
		}); err != nil {
			return err
		}
	}

	for rkey, a := range routes {
		if err := upsertRouteBucket(db, routeKeys[rkey].bucket(a, bucketStart)); err != nil {
			return err
		}
	}

	for k := range attrs {
		if err := upsertAttributeKey(db, k[0], k[1], k[2]); err != nil {
			return err
		}
	}

	if bucketStart.Hour() == 23 {
		dayStart := bucketStart.Truncate(24 * time.Hour)
		if err := aggregateDailyMetrics(db, dayStart); err != nil {
			return err
		}
		if err := aggregateDailyRoutes(db, dayStart, true); err != nil {
			return err
		}
	}

	return nil
}

// splitMetricKey re-derives the (user, project) pair from the map key used
// during the scan. user ids never contain "|" and neither do project names,
// so the split is unambiguous — and it is asserted by a test.
func splitMetricKey(key string, _ *scanMetricAccum) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// percentilesOf computes the three stored percentiles from every duration in
// a bucket, using the same continuous definition as Postgres
// percentile_cont: linear interpolation between the two neighbouring values.
// Without the interpolation the object-store path would drift from the SQL
// path by a millisecond or two on small buckets, which is exactly the kind
// of difference that makes two stores impossible to compare.
func percentilesOf(durations []int64) (p50, p95, p99 int64) {
	if len(durations) == 0 {
		return 0, 0, 0
	}
	sorted := make([]int64, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return int64(percentileCont(sorted, 0.5)),
		int64(percentileCont(sorted, 0.95)),
		int64(percentileCont(sorted, 0.99))
}

// percentileCont is Postgres percentile_cont over an ascending slice.
func percentileCont(sorted []int64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return float64(sorted[0])
	}
	rank := p * float64(n-1)
	lower := int(rank)
	if lower >= n-1 {
		return float64(sorted[n-1])
	}
	frac := rank - float64(lower)
	return float64(sorted[lower]) + frac*float64(sorted[lower+1]-sorted[lower])
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// StartAggregationWorker runs aggregation for the previous full hour at
// startup, then every hour. Buckets are in UTC.
//
// scanner selects the driver: nil reads the Postgres events table with SQL,
// non-nil streams from that store. startupHours bounds the startup catch-up
// because on an object store every one of those hours is a download.
func StartAggregationWorker(db *gorm.DB, scanner EventScanner, startupHours int) {
	go func() {
		// The status-count backfill repairs hourly route buckets written
		// before the column existed. With a scanner the events table is
		// empty, and the scanner driver always writes status_counts, so the
		// backfill has nothing to do and is skipped rather than run once per
		// stale row against an empty table.
		if scanner == nil {
			backfillRouteBucketStatuses(db)
		}
		backfillDailyBuckets(db, scanner != nil)

		if startupHours < 0 {
			startupHours = 0
		}
		if scanner != nil {
			log.Printf("aggregation: rebuilding rollups from the configured object store, %d hour(s) back", startupHours)
		}

		// Run for the last startupHours completed hours at startup.
		now := time.Now().UTC()
		for i := 1; i <= startupHours; i++ {
			bucketStart := now.Truncate(time.Hour).Add(-time.Duration(i) * time.Hour)
			if err := runAggregation(db, scanner, bucketStart); err != nil {
				log.Printf("aggregation error (startup) for %s: %v", bucketStart.Format(time.RFC3339), err)
			}
		}

		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for t := range ticker.C {
			bucketStart := t.UTC().Truncate(time.Hour).Add(-time.Hour)
			if err := runAggregation(db, scanner, bucketStart); err != nil {
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
		UserID     string
		Project    string
		TotalCount int64
		ErrorCount int64
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
//
// statusFromBuckets selects where the day's status histogram comes from. On
// Postgres it is read from the raw events (a full-day scan, but the
// continuous aggregate makes it cheap); when the events live in an object
// store there is nothing to scan, so the hourly route buckets — which are in
// Postgres either way — are merged instead. The merged answer is the same
// histogram, summed over the day's 24 buckets.
func aggregateDailyRoutes(db *gorm.DB, dayStart time.Time, statusFromBuckets bool) error {
	dayEnd := dayStart.Add(24 * time.Hour)

	type dailyRow struct {
		UserID     string
		Project    string
		Route      string
		TotalCount int64
		ErrorCount int64
		DurSum     float64
		Count      int64
		StatusJSON string
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

	// One query for the whole day's status histogram, keyed by route, rather
	// than a query per route.
	var statusByRoute map[string]map[int]int64
	if statusFromBuckets {
		var err error
		statusByRoute, err = statusCountsFromRouteBuckets(db, dayStart, dayEnd)
		if err != nil {
			return err
		}
	}

	for _, r := range rows {
		avgDur := 0.0
		if r.Count > 0 {
			avgDur = r.DurSum / float64(r.Count)
		}

		statusJSON := map[string]interface{}{}
		if statusFromBuckets {
			for status, count := range statusByRoute[r.UserID+"|"+r.Project+"|"+r.Route] {
				statusJSON[fmt.Sprint(status)] = count
			}
		} else {
			// Compute status counts directly from events for the day
			// (more reliable than merging hourly JSON blobs)
			type scRow struct {
				Status int
				Count  int64
			}
			var daySCs []scRow
			var statusErr error
			if UseContinuousAggregates() {
				// The routes cagg already carries the status dimension, so the
				// day's status histogram comes from a few hundred pre-aggregated
				// rows rather than a full-day scan of raw events.
				statusErr = db.Raw(`
					SELECT status, SUM(count) AS count
					FROM `+continuousAggregateRoutes+`
					WHERE user_id = ? AND project = ? AND route = ?
					  AND bucket_start >= ? AND bucket_start < ?
					GROUP BY status
				`, r.UserID, r.Project, r.Route, dayStart, dayEnd).Scan(&daySCs).Error
			} else {
				statusErr = db.Model(&Event{}).
					Select("status, COUNT(*) AS count").
					Where("user_id = ? AND project = ? AND route = ? AND created_at >= ? AND created_at < ?",
						r.UserID, r.Project, r.Route, dayStart, dayEnd).
					Group("status").
					Scan(&daySCs).Error
			}
			if statusErr != nil {
				return statusErr
			}
			for _, sc := range daySCs {
				statusJSON[fmt.Sprint(sc.Status)] = sc.Count
			}
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

// statusCountsFromRouteBuckets merges the hourly status histograms of a day
// into one histogram per route. It reads only the route bucket table, so it
// works identically whether raw events live in Postgres or an object store.
func statusCountsFromRouteBuckets(db *gorm.DB, dayStart, dayEnd time.Time) (map[string]map[int]int64, error) {
	type bucketRow struct {
		UserID       string
		Project      string
		Route        string
		StatusCounts map[string]int64
	}
	var rows []bucketRow
	if err := db.Raw(`
		SELECT user_id, project, route, status_counts
		FROM route_buckets
		WHERE bucket_start >= ? AND bucket_start < ?
	`, dayStart, dayEnd).Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]map[int]int64, len(rows))
	for _, r := range rows {
		key := r.UserID + "|" + r.Project + "|" + r.Route
		m, ok := out[key]
		if !ok {
			m = map[int]int64{}
			out[key] = m
		}
		for statusText, count := range r.StatusCounts {
			var status int
			if _, err := fmt.Sscanf(statusText, "%d", &status); err != nil {
				continue
			}
			m[status] += count
		}
	}
	return out, nil
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

// backfillDailyBuckets computes DailyMetricBucket and DailyRouteBucket rows for
// all prior days that don't yet have them, by rolling up existing hourly buckets.
func backfillDailyBuckets(db *gorm.DB, statusFromBuckets bool) {
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
				if err := aggregateDailyRoutes(db, dayStart, statusFromBuckets); err != nil {
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
