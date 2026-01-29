package handlers

import (
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

var safeAttrKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// parseRange reads "hours" (float, e.g. 0.5 or 1) or "days" (int) from query and returns
// cutoff time and, for traffic, whether to use 30-min buckets (true when range <= 2 hours).
func parseRange(ctx *fasthttp.RequestCtx) (cutoff time.Time, bucket30Min bool) {
	now := time.Now()
	if h := string(ctx.QueryArgs().Peek("hours")); h != "" {
		if f, err := strconv.ParseFloat(h, 64); err == nil && f > 0 {
			cutoff = now.Add(-time.Duration(f * float64(time.Hour)))
			bucket30Min = f <= 2
			return cutoff, bucket30Min
		}
	}
	days := 0
	if d := string(ctx.QueryArgs().Peek("days")); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			days = n
		}
	}
	if days == 0 {
		days = 1
	}
	cutoff = now.Add(-time.Duration(days) * 24 * time.Hour)
	return cutoff, false
}

// RequestLogger returns fasthttp middleware that logs method, path, status, duration.
func RequestLogger(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		start := time.Now()
		next(ctx)
		log.Printf("%s %s -> %d (%s) ip=%s", ctx.Method(), ctx.Path(), ctx.Response.StatusCode(), time.Since(start), ctx.RemoteAddr())
	}
}

func jsonResponse(ctx *fasthttp.RequestCtx, data map[string]any) {
	ctx.SetContentType("application/json")
	body, _ := json.Marshal(data)
	ctx.SetBody(body)
}

func errResponse(ctx *fasthttp.RequestCtx, code int, msg string) {
	ctx.SetStatusCode(code)
	ctx.SetBodyString(msg)
}

type trafficPoint struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

type statusCount struct {
	Status int   `json:"status"`
	Count  int64 `json:"count"`
}

type topRoute struct {
	Route    string        `json:"route"`
	Count    int64         `json:"count"`
	Statuses []statusCount `json:"statuses,omitempty" gorm:"-"`
}

func applyMetricsFilters(q *gorm.DB, status, attrKey, attrValue string) *gorm.DB {
	switch status {
	case "success":
		q = q.Where("status < ?", 400)
	case "error":
		q = q.Where("status >= ?", 400)
	}
	if attrKey != "" && attrValue != "" && safeAttrKey.MatchString(attrKey) {
		q = q.Where("attributes::jsonb ->> ? = ?", attrKey, attrValue)
	}
	return q
}

func TrafficSeries(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, bucket30Min := parseRange(ctx)
		status := string(ctx.QueryArgs().Peek("status"))
		attrKey := string(ctx.QueryArgs().Peek("attr_key"))
		attrValue := string(ctx.QueryArgs().Peek("attr_value"))

		// Use Raw so GROUP BY is never parameterized. Bucket by 30 min for short ranges, else 1 hour.
		var bucketExpr string
		if bucket30Min {
			bucketExpr = `to_char(to_timestamp(floor(extract(epoch from created_at) / 1800) * 1800), 'YYYY-MM-DD"T"HH24:MI:SS') || 'Z'`
		} else {
			bucketExpr = `to_char(date_trunc('hour', created_at), 'YYYY-MM-DD"T"HH24:MI:SS') || 'Z'`
		}
		sql := `SELECT ` + bucketExpr + ` AS bucket, count(*) AS count FROM events WHERE user_id = ? AND created_at >= ?`
		args := []any{strconv.Itoa(int(user.ID)), cutoff}
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
			sql += ` AND attributes::jsonb ->> ? = ?`
			args = append(args, attrKey, attrValue)
		}
		if bucket30Min {
			sql += ` GROUP BY floor(extract(epoch from created_at) / 1800) ORDER BY 1`
		} else {
			sql += ` GROUP BY date_trunc('hour', created_at) ORDER BY 1`
		}

		var rows []trafficPoint
		if err := db.Raw(sql, args...).Scan(&rows).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query metrics")
			return
		}
		jsonResponse(ctx, map[string]any{"series": rows})
	}
}

func TopRoutes(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, _ := parseRange(ctx)
		status := string(ctx.QueryArgs().Peek("status"))
		attrKey := string(ctx.QueryArgs().Peek("attr_key"))
		attrValue := string(ctx.QueryArgs().Peek("attr_value"))

		limit := 10
		if s := string(ctx.QueryArgs().Peek("limit")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 100 {
					n = 100
				}
				limit = n
			}
		}
		offset := 0
		if s := string(ctx.QueryArgs().Peek("offset")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				offset = n
			}
		}

		q := db.Model(&dbpkg.Event{}).
			Where("user_id = ?", strconv.Itoa(int(user.ID))).
			Where("created_at >= ?", cutoff)
		if project != "" {
			q = q.Where("project = ?", project)
		}
		q = applyMetricsFilters(q, status, attrKey, attrValue)

		var totalCount int64
		if err := q.Select("COUNT(DISTINCT route)").Scan(&totalCount).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to count routes")
			return
		}

		var rows []topRoute
		if err := q.
			Select("route as route, count(*) as count").
			Group("route").
			Order("count(*) DESC").
			Limit(limit).
			Offset(offset).
			Scan(&rows).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query top routes")
			return
		}

		if len(rows) > 0 {
			routeNames := make([]string, 0, len(rows))
			for _, row := range rows {
				if row.Route != "" {
					routeNames = append(routeNames, row.Route)
				}
			}
			if len(routeNames) > 0 {
				type scRow struct {
					Route  string
					Status int
					Count  int64
				}
				var scRows []scRow
				qs := db.Model(&dbpkg.Event{}).
					Where("user_id = ?", strconv.Itoa(int(user.ID))).
					Where("created_at >= ?", cutoff)
				if project != "" {
					qs = qs.Where("project = ?", project)
				}
				qs = applyMetricsFilters(qs, status, attrKey, attrValue)
				if err := qs.
					Where("route IN ?", routeNames).
					Select("route as route, status as status, count(*) as count").
					Group("route, status").
					Scan(&scRows).Error; err != nil {
					errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query route status breakdown")
					return
				}
				byRoute := make(map[string][]statusCount, len(routeNames))
				for _, sc := range scRows {
					byRoute[sc.Route] = append(byRoute[sc.Route], statusCount{Status: sc.Status, Count: sc.Count})
				}
				for i := range rows {
					rows[i].Statuses = byRoute[rows[i].Route]
				}
			}
		}

		hasMore := offset+limit < int(totalCount)
		jsonResponse(ctx, map[string]any{"routes": rows, "total": totalCount, "has_more": hasMore})
	}
}

type recentEvent struct {
	ID         uint   `json:"id"`
	Time       string `json:"time"`        // legacy, pre-formatted server time
	CreatedAt  string `json:"created_at"`  // ISO 8601 UTC for client-side local formatting
	Method     string `json:"method"`
	Route      string `json:"route"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Project    string `json:"project"`
}

func RecentEvents(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		status := string(ctx.QueryArgs().Peek("status"))
		attrKey := string(ctx.QueryArgs().Peek("attr_key"))
		attrValue := string(ctx.QueryArgs().Peek("attr_value"))

		limit := 10
		if s := string(ctx.QueryArgs().Peek("limit")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 200 {
					n = 200
				}
				limit = n
			}
		}
		offset := 0
		if s := string(ctx.QueryArgs().Peek("offset")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				offset = n
			}
		}

		q := db.Model(&dbpkg.Event{}).Where("user_id = ?", strconv.Itoa(int(user.ID)))
		if project != "" {
			q = q.Where("project = ?", project)
		}
		q = applyMetricsFilters(q, status, attrKey, attrValue)

		var totalCount int64
		if err := q.Count(&totalCount).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to count events")
			return
		}

		var events []dbpkg.Event
		if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query recent events")
			return
		}

		timeFormat := "12"
		if user.TimeFormat != "" {
			timeFormat = user.TimeFormat
		}
		rows := make([]recentEvent, 0, len(events))
		for _, e := range events {
			rows = append(rows, recentEvent{
				ID:         e.ID,
				Time:       FormatEventTime(e.CreatedAt, timeFormat),
				CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
				Method:     e.Method,
				Route:      e.Route,
				Status:     e.Status,
				DurationMs: e.DurationMs,
				Project:    e.Project,
			})
		}

		hasMore := offset+limit < int(totalCount)
		jsonResponse(ctx, map[string]any{"events": rows, "total": totalCount, "has_more": hasMore})
	}
}

func ErrorRateSeries(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, _ := parseRange(ctx)
		cutoff = cutoff.UTC()

		q := db.Model(&dbpkg.MetricBucket{}).Where("user_id = ?", strconv.Itoa(int(user.ID))).Where("bucket_start >= ?", cutoff)
		if project != "" {
			q = q.Where("project = ?", project)
		}

		var buckets []dbpkg.MetricBucket
		if err := q.Order("bucket_start").Find(&buckets).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query error rate")
			return
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
		jsonResponse(ctx, map[string]any{"series": series})
	}
}

func LatencyPercentilesSeries(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, _ := parseRange(ctx)
		cutoff = cutoff.UTC()

		q := db.Model(&dbpkg.MetricBucket{}).Where("user_id = ?", strconv.Itoa(int(user.ID))).Where("bucket_start >= ?", cutoff)
		if project != "" {
			q = q.Where("project = ?", project)
		}

		var buckets []dbpkg.MetricBucket
		if err := q.Order("bucket_start").Find(&buckets).Error; err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query latency percentiles")
			return
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
		jsonResponse(ctx, map[string]any{"series": series})
	}
}

// AvgDuration returns the average request duration (in milliseconds) over the selected range.
// The range is controlled by the same hours/days parameters as other metrics endpoints.
func AvgDuration(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		status := string(ctx.QueryArgs().Peek("status"))
		attrKey := string(ctx.QueryArgs().Peek("attr_key"))
		attrValue := string(ctx.QueryArgs().Peek("attr_value"))
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
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query avg duration")
			return
		}
		jsonResponse(ctx, map[string]any{"avg_duration_ms": avgDurationMs})
	}
}

func AttributeKeys(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, _ := parseRange(ctx)

		type keyRow struct {
			Key string `json:"key"`
		}
		var rows []keyRow
		var err error
		if project != "" {
			err = db.Raw(
				"SELECT DISTINCT je.key AS key FROM events, jsonb_each(events.attributes::jsonb) je WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ?",
				strconv.Itoa(int(user.ID)), project, cutoff,
			).Scan(&rows).Error
		} else {
			err = db.Raw(
				"SELECT DISTINCT je.key AS key FROM events, jsonb_each(events.attributes::jsonb) je WHERE events.user_id = ? AND events.created_at >= ?",
				strconv.Itoa(int(user.ID)), cutoff,
			).Scan(&rows).Error
		}
		if err != nil {
			errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query attribute keys")
			return
		}

		keys := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.Key != "" {
				keys = append(keys, row.Key)
			}
		}
		jsonResponse(ctx, map[string]any{"keys": keys})
	}
}

func AttributeValues(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		attrKey := string(ctx.QueryArgs().Peek("key"))
		if attrKey == "" || !safeAttrKey.MatchString(attrKey) {
			errResponse(ctx, fasthttp.StatusBadRequest, "invalid or missing key")
			return
		}

		project := string(ctx.QueryArgs().Peek("project"))
		cutoff, _ := parseRange(ctx)

		type valRow struct {
			Value string `json:"value"`
		}
		var rows []valRow
		if project != "" {
			err := db.Raw(
				"SELECT DISTINCT events.attributes::jsonb ->> ? AS value FROM events WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ? AND events.attributes::jsonb ? ?",
				attrKey, strconv.Itoa(int(user.ID)), project, cutoff, attrKey,
			).Scan(&rows).Error
			if err != nil {
				errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query attribute values")
				return
			}
		} else {
			err := db.Raw(
				"SELECT DISTINCT events.attributes::jsonb ->> ? AS value FROM events WHERE events.user_id = ? AND events.created_at >= ? AND events.attributes::jsonb ? ?",
				attrKey, strconv.Itoa(int(user.ID)), cutoff, attrKey,
			).Scan(&rows).Error
			if err != nil {
				errResponse(ctx, fasthttp.StatusInternalServerError, "failed to query attribute values")
				return
			}
		}

		values := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.Value != "" {
				values = append(values, row.Value)
			}
		}
		jsonResponse(ctx, map[string]any{"values": values})
	}
}
