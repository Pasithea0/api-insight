package metrics

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	"apiinsight/internal/cache"
	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

type trafficPoint struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

func TrafficSeries(db *gorm.DB, phCache *cache.PartialHourCache, raw *dbpkg.RawEvents) fiber.Handler {
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

		scanner, _ := raw.DefaultScanner()

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

			// Get data for the current partial hour from the in-memory cache
			// instead of hitting the raw events table.
			lastBucketTime := cutoff.UTC().Truncate(time.Hour)
			if len(buckets) > 0 {
				if t, err := time.Parse("2006-01-02T15:04:05Z", buckets[len(buckets)-1].Bucket); err == nil {
					lastBucketTime = t.Add(time.Hour)
				}
			}

			// Only use cache for the current partial hour (not historical catch-up)
			now := time.Now().UTC()
			currentHourStart := now.Truncate(time.Hour)
			if currentHourStart.Equal(lastBucketTime) && phCache.HasRecentData() {
				phData := phCache.Get()
				if count, ok := partialHourTraffic(phData, userID, project, currentHourStart); ok {
					buckets = append(buckets, trafficPoint{
						Bucket: currentHourStart.Format("2006-01-02T15:00:00") + "Z",
						Count:  count,
					})
				}
			} else if scanner != nil {
				// Object-store mode: there is no events table to query, so
				// the current hour is re-read from the store. It is at most
				// one hour of data, and it only happens when the in-memory
				// view does not cover the hour the buckets stop at.
				points, err := scanTrafficPoints(ctx.Context(), scanner, lastBucketTime,
					dbpkg.ScanFilter{UserID: userID, Project: project, Status: status}, false)
				if err == nil {
					for _, p := range points {
						if p.Bucket == bucketLabel(currentHourStart) {
							buckets = append(buckets, p)
						}
					}
				}
			} else {
				// Fallback to DB query if cache is unavailable
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
			}

			return jsonResponse(ctx, map[string]any{"series": buckets})
		}

		// Complex filters or 30-min buckets: the rollup buckets cannot
		// answer these, so the events themselves are read.
		filter := dbpkg.ScanFilter{
			UserID:  userID,
			Project: project,
			Status:  status,
		}
		if attrKey != "" && attrValue != "" && safeAttrKey.MatchString(attrKey) {
			filter.AttrKey = attrKey
			filter.AttrValue = attrValue
		}

		if scanner != nil {
			points, err := scanTrafficPoints(ctx.Context(), scanner, cutoff, filter, bucket30Min)
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query metrics")
			}
			return jsonResponse(ctx, map[string]any{"series": points})
		}

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

// partialHourTraffic sums the cached partial-hour traffic for a scope,
// reproducing what the two branches of the old cache lookup did: an exact
// key when both user and project are known, and a scan across matching keys
// when the request is for "everything" (an admin's all-projects view).
func partialHourTraffic(phData *cache.PartialHourData, userID, project string, hourStart time.Time) (int64, bool) {
	if phData == nil {
		return 0, false
	}
	if userID != "" && project != "" {
		if c, ok := phData.TrafficBy30[userID+"|"+project+"|"+hourStart.Format("2006-01-02T15:00:00")+"Z"]; ok {
			return c, true
		}
		return 0, false
	}
	var total int64
	for k, c := range phData.TrafficBy30 {
		if matchesScope(k, userID, project) {
			total += c
		}
	}
	return total, total > 0
}

// partialHourAverageDuration returns the cached partial-hour mean duration
// and its event count for a scope, used by the avg-duration panel when there
// is no events table to average over.
func partialHourAverageDuration(phData *cache.PartialHourData, userID, project string) (float64, int64) {
	if phData == nil {
		return 0, 0
	}
	var sum float64
	var count int64
	for k, n := range phData.TotalCount {
		if !matchesScope(k, userID, project) {
			continue
		}
		count += n
		sum += phData.DurationSum[k]
	}
	if count == 0 {
		return 0, 0
	}
	return sum / float64(count), count
}

// matchesScope checks if a cache key (format "userID|project|...") matches
// the requested scope. Empty userID or project means "match any".
func matchesScope(cacheKey, userID, project string) bool {
	parts := split2(cacheKey, "|")
	if len(parts) < 2 {
		return false
	}
	if userID != "" && parts[0] != userID {
		return false
	}
	if project != "" && parts[1] != project {
		return false
	}
	return true
}

// split2 splits a string on the first occurrence of sep, returning max 2 parts.
func split2(s, sep string) []string {
	for i := 0; i < len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}
