package metrics

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

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
func AvgDuration(db *gorm.DB) fiber.Handler {
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
			avgDuration, err := avgDurationFromBuckets(db, userID, project, status, cutoff)
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query avg duration")
			}
			return jsonResponse(ctx, map[string]any{"avg_duration_ms": avgDuration})
		}

		// Fallback: compute from raw events
		return avgDurationFromEvents(ctx, db, userID, project, status, attrKey, attrValue, cutoff)
	}
}

func avgDurationFromBuckets(db *gorm.DB, userID, project, status string, cutoff time.Time) (float64, error) {
	bucketCutoff := cutoff.UTC().Truncate(time.Hour)

	// Use SQL to compute weighted average from route_buckets
	sb := `SELECT COALESCE(SUM(avg_duration_ms * total_count), 0) / NULLIF(SUM(total_count), 0)`
	if status == "success" {
		sb += ` FILTER (WHERE total_count > error_count)`
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

	// Partial hour from events (at most 1 hour of data)
	currentHourStart := time.Now().UTC().Truncate(time.Hour)
	partialStart := currentHourStart
	if partialStart.Before(cutoff) {
		partialStart = cutoff
	}
	if partialStart.Before(time.Now().UTC()) {
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
			// Get bucket total count for weighted average
			var bucketTotalCount int64
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
			if err := db.Raw(countQ, countArgs...).Scan(&bucketTotalCount).Error; err != nil {
				return 0, err
			}
			totalCount := bucketTotalCount + partialCount
			if totalCount > 0 {
				bucketAvg = (bucketAvg*float64(bucketTotalCount) + partialAvg*float64(partialCount)) / float64(totalCount)
			}
		}
	}

	return bucketAvg, nil
}

func avgDurationFromEvents(ctx *fiber.Ctx, db *gorm.DB, userID, project, status, attrKey, attrValue string, cutoff time.Time) error {
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
