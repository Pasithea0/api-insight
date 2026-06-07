package metrics

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
	"apiinsight/internal/http/handlers"
)

type statusCount struct {
	Status int   `json:"status"`
	Count  int64 `json:"count"`
}

type topRoute struct {
	Route    string        `json:"route"`
	Count    int64         `json:"count"`
	Statuses []statusCount `json:"statuses,omitempty" gorm:"-"`
}

type topRoutesQuery struct {
	OwnerUserID string
	Project     string
	Cutoff      time.Time
	Status      string
	AttrKey     string
	AttrValue   string
	Limit       int
	Offset      int
}

type topRoutesResult struct {
	Routes  []topRoute
	Total   int64
	HasMore bool
}

func parsePositiveInt(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if max > 0 && value > max {
		return max
	}
	return value
}

func normalizeStatusFilter(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "success":
		return "success"
	case "error":
		return "error"
	default:
		return ""
	}
}

func requestedRangeLabel(ctx *fiber.Ctx) string {
	if raw := strings.ToLower(strings.TrimSpace(ctx.Query("range"))); raw != "" {
		if _, _, ok := parseCompactRange(raw, time.Now()); ok {
			return raw
		}
	}
	if hours := strings.TrimSpace(ctx.Query("hours")); hours != "" {
		return hours + "h"
	}
	if days := strings.TrimSpace(ctx.Query("days")); days != "" {
		return days + "d"
	}
	return "1d"
}

func queryTopRoutes(db *gorm.DB, params topRoutesQuery) (topRoutesResult, error) {
	hasAttrFilter := params.AttrKey != "" && params.AttrValue != ""

	// Fast path: use pre-aggregated RouteBucket data (no attribute filters)
	if !hasAttrFilter {
		return queryTopRoutesFromBuckets(db, params)
	}

	// Fallback: use raw events for attribute-filtered queries
	return queryTopRoutesFromEvents(db, params)
}

func queryTopRoutesFromBuckets(db *gorm.DB, params topRoutesQuery) (topRoutesResult, error) {
	bucketCutoff := params.Cutoff.UTC().Truncate(time.Hour)
	limit := params.Limit
	offset := params.Offset

	// Aggregate route_buckets in SQL — fast GROUP BY on small table
	countField := "SUM(total_count) as count"
	errorField := "SUM(error_count) as error_count"
	if params.Status == "success" {
		countField = "SUM(total_count - error_count) as count"
	} else if params.Status == "error" {
		countField = "SUM(error_count) as count"
		errorField = "SUM(error_count) as error_count"
	}

	type bucketAggRow struct {
		Route      string
		Count      int64
		ErrorCount int64 `gorm:"column:error_count"`
	}
	var bucketRows []bucketAggRow
	q := db.Model(&dbpkg.RouteBucket{}).
		Select("route, "+countField+", "+errorField).
		Where("bucket_start >= ?", bucketCutoff)
	q = scopeQueryUserID(q, params.OwnerUserID)
	if params.Project != "" {
		q = q.Where("project = ?", params.Project)
	}
	if err := q.Group("route").Order("count DESC").Limit(limit + 1).Offset(offset).Scan(&bucketRows).Error; err != nil {
		return topRoutesResult{}, err
	}

	hasMore := len(bucketRows) > limit
	if hasMore {
		bucketRows = bucketRows[:limit]
	}
	if len(bucketRows) == 0 {
		return topRoutesResult{Routes: []topRoute{}, HasMore: false}, nil
	}

	// Fetch status_counts for just the top N routes (merge across hours in Go)
	routeNames := make([]string, len(bucketRows))
	routeIdx := make(map[string]int, len(bucketRows))
	for i, r := range bucketRows {
		routeNames[i] = r.Route
		routeIdx[r.Route] = i
	}

	type scBucketRow struct {
		Route       string
		StatusCounts datatypes.JSONMap
	}
	var scRows []scBucketRow
	sq := db.Model(&dbpkg.RouteBucket{}).
		Select("route, status_counts").
		Where("bucket_start >= ?", bucketCutoff).
		Where("route IN ?", routeNames)
	sq = scopeQueryUserID(sq, params.OwnerUserID)
	if params.Project != "" {
		sq = sq.Where("project = ?", params.Project)
	}
	if err := sq.Find(&scRows).Error; err != nil {
		return topRoutesResult{}, err
	}

	type routeMeta struct {
		route    string
		count    int64
		statuses map[int]int64
	}
	meta := make([]routeMeta, len(bucketRows))
	for i, r := range bucketRows {
		m := routeMeta{route: r.Route, count: r.Count, statuses: make(map[int]int64)}
		meta[i] = m
	}
	for _, sc := range scRows {
		idx, ok := routeIdx[sc.Route]
		if !ok {
			continue
		}
		for k, v := range sc.StatusCounts {
			if fv, ok := v.(float64); ok {
				statusInt, _ := strconv.Atoi(k)
				if statusInt > 0 {
					meta[idx].statuses[statusInt] += int64(fv)
				}
			}
		}
	}

	// Fallback: if any route has no statuses from route_buckets, compute from events
	var fallbackRoutes []string
	for _, m := range meta {
		if len(m.statuses) == 0 && m.count > 0 {
			fallbackRoutes = append(fallbackRoutes, m.route)
		}
	}
	if len(fallbackRoutes) > 0 {
		type fsRow struct {
			Route  string
			Status int
			Count  int64
		}
		var fsRows []fsRow
		fq := db.Model(&dbpkg.Event{}).
			Where("created_at >= ?", params.Cutoff).
			Where("route IN ?", fallbackRoutes)
		fq = scopeQueryUserID(fq, params.OwnerUserID)
		if params.Project != "" {
			fq = fq.Where("project = ?", params.Project)
		}
		if err := fq.Select("route, status, COUNT(*) as count").
			Group("route, status").Scan(&fsRows).Error; err != nil {
			return topRoutesResult{}, err
		}
		for _, r := range fsRows {
			if idx, ok := routeIdx[r.Route]; ok {
				meta[idx].statuses[r.Status] += r.Count
			}
		}
	}

	// Build response
	rows := make([]topRoute, 0, len(meta))
	for _, m := range meta {
		if m.count <= 0 {
			continue
		}
		sc := make([]statusCount, 0, len(m.statuses))
		for status, count := range m.statuses {
			if params.Status == "success" && status >= 400 {
				continue
			}
			if params.Status == "error" && status < 400 {
				continue
			}
			sc = append(sc, statusCount{Status: status, Count: count})
		}
		rows = append(rows, topRoute{
			Route:    m.route,
			Count:    m.count,
			Statuses: sc,
		})
	}

	return topRoutesResult{
		Routes:  rows,
		HasMore: hasMore,
	}, nil
}

func queryTopRoutesFromEvents(db *gorm.DB, params topRoutesQuery) (topRoutesResult, error) {
	q := db.Model(&dbpkg.Event{}).
		Where("created_at >= ?", params.Cutoff)
	q = scopeQueryUserID(q, params.OwnerUserID)
	if params.Project != "" {
		q = q.Where("project = ?", params.Project)
	}
	q = applyMetricsFilters(q, params.Status, params.AttrKey, params.AttrValue)

	var rows []topRoute
	if err := q.
		Select("route as route, count(*) as count").
		Group("route").
		Order("count(*) DESC").
		Limit(params.Limit + 1).
		Offset(params.Offset).
		Scan(&rows).Error; err != nil {
		return topRoutesResult{}, err
	}

	hasMore := false
	if len(rows) > params.Limit {
		hasMore = true
		rows = rows[:params.Limit]
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
				Where("created_at >= ?", params.Cutoff)
			qs = scopeQueryUserID(qs, params.OwnerUserID)
			if params.Project != "" {
				qs = qs.Where("project = ?", params.Project)
			}
			qs = applyMetricsFilters(qs, params.Status, params.AttrKey, params.AttrValue)
			if err := qs.
				Where("route IN ?", routeNames).
				Select("route as route, status as status, count(*) as count").
				Group("route, status").
				Scan(&scRows).Error; err != nil {
				return topRoutesResult{}, err
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

	return topRoutesResult{
		Routes:  rows,
		HasMore: hasMore,
	}, nil
}

func TopRoutes(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		status := normalizeStatusFilter(ctx.Query("status"))
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")
		limit := parsePositiveInt(ctx.Query("limit"), 10, 100)
		offset := 0
		if s := ctx.Query("offset"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				offset = n
			}
		}

		result, err := queryTopRoutes(db, topRoutesQuery{
			OwnerUserID: userID,
			Project:     project,
			Cutoff:      cutoff,
			Status:      status,
			AttrKey:     attrKey,
			AttrValue:   attrValue,
			Limit:       limit,
			Offset:      offset,
		})
		if err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to count routes")
		}

		return jsonResponse(ctx, map[string]any{
			"routes":   result.Routes,
			"total":    result.Total,
			"has_more": result.HasMore,
		})
	}
}

func PublicTopRoutes(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		apiKey, ok := httpctx.APIKeyFromCtx(ctx)
		if !ok || apiKey == nil {
			return ctx.Status(fiber.StatusUnauthorized).SendString("unauthorized")
		}

		cutoff, _ := parseRange(ctx)
		status := normalizeStatusFilter(ctx.Query("status"))
		count := parsePositiveInt(ctx.Query("count"), 20, 100)

		result, err := queryTopRoutes(db, topRoutesQuery{
			OwnerUserID: strconv.Itoa(int(apiKey.UserID)),
			Project:     apiKey.Name,
			Cutoff:      cutoff,
			Status:      status,
			Limit:       count,
			Offset:      0,
		})
		if err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query top routes")
		}

		statusLabel := status
		if statusLabel == "" {
			statusLabel = "all"
		}

		return jsonResponse(ctx, map[string]any{
			"project":    apiKey.Name,
			"range":      requestedRangeLabel(ctx),
			"status":     statusLabel,
			"count":      count,
			"routes":     result.Routes,
			"total":      result.Total,
			"has_more":   result.HasMore,
			"public_api": true,
		})
	}
}
