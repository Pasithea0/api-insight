package metrics

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
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
	q := db.Model(&dbpkg.Event{}).
		Where("user_id = ?", params.OwnerUserID).
		Where("created_at >= ?", params.Cutoff)
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

	// We no longer calculate total distinct routes as it is too slow.
	// We return 0 for total, the frontend can rely on has_more.
	var totalCount int64 = 0

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
				Where("user_id = ?", params.OwnerUserID).
				Where("created_at >= ?", params.Cutoff)
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
		Total:   totalCount,
		HasMore: hasMore,
	}, nil
}

func TopRoutes(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
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
			OwnerUserID: strconv.Itoa(int(user.ID)),
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
