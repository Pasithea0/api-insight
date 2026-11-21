package metrics

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	"apiinsight/internal/cache"
	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

func LatencyPercentilesSeries(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		cutoff = cutoff.UTC()

		q := db.Model(&dbpkg.MetricBucket{}).Where("bucket_start >= ?", cutoff)
		q = scopeQueryUserID(q, userID)
		if project != "" {
			q = q.Where("project = ?", project)
		}

		var buckets []dbpkg.MetricBucket
		if err := q.Order("bucket_start").Find(&buckets).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query latency percentiles")
		}

		series := make([]map[string]any, 0, len(buckets))
		for _, b := range buckets {
			utc := time.Date(b.BucketStart.Year(), b.BucketStart.Month(), b.BucketStart.Day(),
				b.BucketStart.Hour(), b.BucketStart.Minute(), b.BucketStart.Second(), 0, time.UTC)
			bucketISO := utc.Format("2006-01-02T15:04:05") + "Z"
			series = append(series, map[string]any{
				"bucket": bucketISO,
				"p50_ms": b.DurationP50Ms,
				"p95_ms": b.DurationP95Ms,
				"p99_ms": b.DurationP99Ms,
			})
		}
		return jsonResponse(ctx, map[string]any{"series": series})
	}
}

// AvgDuration returns the average request duration (in milliseconds) over the selected range.
// The range is controlled by the same hours/days parameters as other metrics endpoints.
func AvgDuration(db *gorm.DB, phCache *cache.PartialHourCache, raw *dbpkg.RawEvents) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		project := ctx.Query("project")
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")
		cutoff, _ := parseRange(ctx)

		hasAttrFilter := attrKey != "" && attrValue != ""

		// Fast path: use pre-aggregated RouteBucket data (no attribute filters)
		if !hasAttrFilter {
			avgDuration, err := avgDurationFromBuckets(db, phCache, raw, userID, project, status, cutoff)
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query avg duration")
			}
			return jsonResponse(ctx, map[string]any{"avg_duration_ms": avgDuration})
		}

		// Fallback: compute from raw events
		return avgDurationFromEvents(ctx, db, raw, userID, project, status, attrKey, attrValue, cutoff)
	}
}

// avgDurationFromBuckets computes the mean over the completed hours from the
// route rollups, and folds in the current partial hour.
//
// The partial hour cannot come from a rollup — the rollup for an incomplete
// hour does not exist yet — so it comes from the in-memory view of the
// current hour when the events live in an object store, and from the events
// table otherwise.
func avgDurationFromBuckets(db *gorm.DB, phCache *cache.PartialHourCache, raw *dbpkg.RawEvents, userID, project, status string, cutoff time.Time) (float64, error) {
	bucketCutoff := cutoff.UTC().Truncate(time.Hour)

	// Use SQL to compute weighted average from route_buckets
	// Filter to success routes when status filter is "success" by filtering
	// after aggregation with a HAVING clause instead of invalid FILTER syntax.
	sb := `SELECT COALESCE(SUM(avg_duration_ms * total_count) / NULLIF(SUM(total_count), 0), 0)`
	if status == "success" {
		sb = `SELECT COALESCE(SUM(avg_duration_ms * (total_count - error_count)) / NULLIF(SUM(total_count - error_count), 0), 0)`
	} else if status == "error" {
		sb = `SELECT COALESCE(SUM(avg_duration_ms * error_count) / NULLIF(SUM(error_count), 0), 0)`
	}
	sb += ` FROM route_buckets WHERE bucket_start >= ?`
	args := []any{bucketCutoff}
	if userID != "" {
		sb += ` AND user_id = ?`
		args = append(args, userID)
	}
	if project != "" {
		sb += ` AND project = ?`
		args = append(args, project)
	}

	var bucketAvg float64
	if err := db.Raw(sb, args...).Scan(&bucketAvg).Error; err != nil {
		return 0, err
	}
	if bucketAvg < 0 {
		bucketAvg = 0
	}

	// Partial hour (at most 1 hour of data)
	currentHourStart := time.Now().UTC().Truncate(time.Hour)
	partialStart := currentHourStart
	if partialStart.Before(cutoff) {
		partialStart = cutoff
	}
	if !partialStart.Before(time.Now().UTC()) {
		return bucketAvg, nil
	}

	// Bucket total, needed to weight the partial hour against the rollups.
	bucketTotalCount, err := bucketEventCount(db, bucketCutoff, userID, project)
	if err != nil {
		return 0, err
	}

	if scanner, _ := raw.DefaultScanner(); scanner != nil {
		// No events table: take the partial hour from the live view that the
		// ingest path maintains. Status is deliberately not applied here —
		// the live view is not split by status, and inventing a filtered
		// number from an unfiltered one would be worse than the small
		// approximation of leaving the partial hour unfiltered, exactly as
		// the SQL branch below does when the filter matches nothing.
		partialAvg, partialCount := partialHourAverageDuration(phCache.Get(), userID, project)
		if partialCount > 0 {
			total := bucketTotalCount + partialCount
			if total > 0 {
				bucketAvg = (bucketAvg*float64(bucketTotalCount) + partialAvg*float64(partialCount)) / float64(total)
			}
		}
		return bucketAvg, nil
	}

	pq := db.Model(&dbpkg.Event{}).
		Where("created_at >= ?", partialStart)
	pq = scopeQueryUserID(pq, userID)
	if project != "" {
		pq = pq.Where("project = ?", project)
	}
	if status == "success" {
		pq = pq.Where("status < 400")
	} else if status == "error" {
		pq = pq.Where("status >= 400")
	}

	var partialAvg float64
	if err := pq.Select("COALESCE(AVG(duration_ms), 0)").Scan(&partialAvg).Error; err != nil {
		return 0, err
	}
	var partialCount int64
	if err := pq.Select("COUNT(*)").Scan(&partialCount).Error; err != nil {
		return 0, err
	}
	if partialCount > 0 {
		totalCount := bucketTotalCount + partialCount
		if totalCount > 0 {
			bucketAvg = (bucketAvg*float64(bucketTotalCount) + partialAvg*float64(partialCount)) / float64(totalCount)
		}
	}

	return bucketAvg, nil
}

// bucketEventCount is how many rolled-up events the range holds. It is the
// weight the partial hour is blended against.
func bucketEventCount(db *gorm.DB, bucketCutoff time.Time, userID, project string) (int64, error) {
	countQ := `SELECT COALESCE(SUM(total_count), 0) FROM route_buckets WHERE bucket_start >= ?`
	countArgs := []any{bucketCutoff}
	if userID != "" {
		countQ += ` AND user_id = ?`
		countArgs = append(countArgs, userID)
	}
	if project != "" {
		countQ += ` AND project = ?`
		countArgs = append(countArgs, project)
	}
	var total int64
	if err := db.Raw(countQ, countArgs...).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func avgDurationFromEvents(ctx *fiber.Ctx, db *gorm.DB, raw *dbpkg.RawEvents, userID, project, status, attrKey, attrValue string, cutoff time.Time) error {
	filter := dbpkg.ScanFilter{
		UserID:    userID,
		Project:   project,
		Status:    status,
		AttrKey:   attrKey,
		AttrValue: attrValue,
	}
	if !safeAttrKey.MatchString(attrKey) {
		// The SQL path silently ignores an unsafe attribute key; do the same
		// rather than failing a request the old code answered.
		filter.AttrKey = ""
		filter.AttrValue = ""
	}

	if scanner, _ := raw.DefaultScanner(); scanner != nil {
		avg, err := scanAverageDuration(ctx.Context(), scanner, cutoff, filter)
		if err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query avg duration")
		}
		return jsonResponse(ctx, map[string]any{"avg_duration_ms": avg})
	}

	q := db.Model(&dbpkg.Event{}).
		Where("created_at >= ?", cutoff)
	q = scopeQueryUserID(q, userID)
	if project != "" {
		q = q.Where("project = ?", project)
	}
	q = applyMetricsFilters(q, status, attrKey, attrValue)

	var avgDurationMs float64
	if err := q.Select("COALESCE(AVG(duration_ms), 0)").Scan(&avgDurationMs).Error; err != nil {
		return errResponse(ctx, fiber.StatusInternalServerError, "failed to query avg duration")
	}
	return jsonResponse(ctx, map[string]any{"avg_duration_ms": avgDurationMs})
}
