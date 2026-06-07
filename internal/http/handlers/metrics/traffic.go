package metrics

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

type trafficPoint struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

func TrafficSeries(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		project := ctx.Query("project")
		cutoff, bucket30Min := parseRange(ctx)
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")

		// If we can use pre-aggregated buckets (no attribute filters and hourly buckets)
		if attrKey == "" && !bucket30Min {
			// Get historical data from buckets
			var buckets []trafficPoint
			q := db.Model(&dbpkg.MetricBucket{}).
				Where("bucket_start >= ?", cutoff.UTC().Truncate(time.Hour))
			q = scopeQueryUserID(q, userID)
			if project != "" {
				q = q.Where("project = ?", project)
			}

			var selectExpr string
			if status == "success" {
				selectExpr = "to_char(bucket_start, 'YYYY-MM-DD\"T\"HH24:MI:SS') || 'Z' as bucket, sum(total_count - error_count) as count"
			} else if status == "error" {
				selectExpr = "to_char(bucket_start, 'YYYY-MM-DD\"T\"HH24:MI:SS') || 'Z' as bucket, sum(error_count) as count"
			} else {
				selectExpr = "to_char(bucket_start, 'YYYY-MM-DD\"T\"HH24:MI:SS') || 'Z' as bucket, sum(total_count) as count"
			}

			if err := q.Select(selectExpr).Group("bucket_start").Order("bucket_start").Scan(&buckets).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query bucket metrics")
			}

			// Also get data for the current partial hour from events table
			lastBucketTime := cutoff.UTC().Truncate(time.Hour)
			if len(buckets) > 0 {
				if t, err := time.Parse("2006-01-02T15:04:05Z", buckets[len(buckets)-1].Bucket); err == nil {
					lastBucketTime = t.Add(time.Hour)
				}
			}

			var currentHour trafficPoint
			cq := db.Model(&dbpkg.Event{}).
				Where("created_at >= ?", lastBucketTime)
			cq = scopeQueryUserID(cq, userID)
			if project != "" {
				cq = cq.Where("project = ?", project)
			}
			if status == "success" {
				cq = cq.Where("status < 400")
			} else if status == "error" {
				cq = cq.Where("status >= 400")
			}

			if err := cq.Select("to_char(date_trunc('hour', created_at), 'YYYY-MM-DD\"T\"HH24:MI:SS') || 'Z' as bucket, count(*) as count").
				Scan(&currentHour).Error; err == nil && currentHour.Bucket != "" {
				buckets = append(buckets, currentHour)
			}

			return jsonResponse(ctx, map[string]any{"series": buckets})
		}

		// Fallback to raw query for complex filters or 30-min buckets
		var bucketExpr string
		if bucket30Min {
			bucketExpr = `to_char(to_timestamp(floor(extract(epoch from created_at) / 1800) * 1800), 'YYYY-MM-DD"T"HH24:MI:SS') || 'Z'`
		} else {
			bucketExpr = `to_char(date_trunc('hour', created_at), 'YYYY-MM-DD"T"HH24:MI:SS') || 'Z'`
		}
		sql := `SELECT ` + bucketExpr + ` AS bucket, count(*) AS count FROM events WHERE created_at >= ?`
		args := []any{cutoff}
		if userID != "" {
			sql += ` AND user_id = ?`
			args = append(args, userID)
		}
		if project != "" {
			sql += ` AND project = ?`
			args = append(args, project)
		}
		if status == "success" {
			sql += ` AND status < 400`
		} else if status == "error" {
			sql += ` AND status >= 400`
		}
		if attrKey != "" && attrValue != "" && safeAttrKey.MatchString(attrKey) {
			sql += ` AND attributes @> CAST(json_build_object(?, ?) AS jsonb)`
			args = append(args, attrKey, attrValue)
		}
		if bucket30Min {
			sql += ` GROUP BY floor(extract(epoch from created_at) / 1800) ORDER BY 1`
		} else {
			sql += ` GROUP BY date_trunc('hour', created_at) ORDER BY 1`
		}

		var rows []trafficPoint
		if err := db.Raw(sql, args...).Scan(&rows).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query metrics")
		}
		return jsonResponse(ctx, map[string]any{"series": rows})
	}
}
