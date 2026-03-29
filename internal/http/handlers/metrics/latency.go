package metrics

import (
	"strconv"
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
		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		cutoff = cutoff.UTC()

		q := db.Model(&dbpkg.MetricBucket{}).Where("user_id = ?", strconv.Itoa(int(user.ID))).Where("bucket_start >= ?", cutoff)
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
		project := ctx.Query("project")
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")
		cutoff, _ := parseRange(ctx)

		q := db.Model(&dbpkg.Event{}).
			Where("user_id = ?", strconv.Itoa(int(user.ID))).
			Where("created_at >= ?", cutoff)
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
}
