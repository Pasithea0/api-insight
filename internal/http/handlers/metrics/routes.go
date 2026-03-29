package metrics

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
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

func TopRoutes(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")

		limit := 10
		if s := ctx.Query("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 100 {
					n = 100
				}
				limit = n
			}
		}
		offset := 0
		if s := ctx.Query("offset"); s != "" {
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
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to count routes")
		}

		var rows []topRoute
		if err := q.
			Select("route as route, count(*) as count").
			Group("route").
			Order("count(*) DESC").
			Limit(limit).
			Offset(offset).
			Scan(&rows).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query top routes")
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
					return errResponse(ctx, fiber.StatusInternalServerError, "failed to query route status breakdown")
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
		return jsonResponse(ctx, map[string]any{"routes": rows, "total": totalCount, "has_more": hasMore})
	}
}
