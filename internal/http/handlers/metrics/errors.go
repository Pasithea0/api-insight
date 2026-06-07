package metrics

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

func ErrorRateSeries(db *gorm.DB) fiber.Handler {
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
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query error rate")
		}

		series := make([]map[string]any, 0, len(buckets))
		for _, b := range buckets {
			rate := 0.0
			if b.TotalCount > 0 {
				rate = float64(b.ErrorCount) / float64(b.TotalCount)
			}
			// BucketStart is stored as UTC; interpret as UTC so frontend gets correct instant for local display.
			utc := time.Date(b.BucketStart.Year(), b.BucketStart.Month(), b.BucketStart.Day(),
				b.BucketStart.Hour(), b.BucketStart.Minute(), b.BucketStart.Second(), 0, time.UTC)
			bucketISO := utc.Format("2006-01-02T15:04:05") + "Z"
			series = append(series, map[string]any{
				"bucket":     bucketISO,
				"error_rate": rate,
				"total":      b.TotalCount,
				"errors":     b.ErrorCount,
			})
		}
		return jsonResponse(ctx, map[string]any{"series": series})
	}
}
