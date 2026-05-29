package metrics

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

func PatternCounts(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		field := ctx.Query("field") // "route", "remote_ip", or an attribute key
		compareBy := ctx.Query("compare_by")
		pattern := ctx.Query("pattern")
		matchType := ctx.Query("type") // "includes", "ends_with", "starts_with"

		if field == "" || pattern == "" {
			return errResponse(ctx, fiber.StatusBadRequest, "missing field or pattern")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		userID := strconv.Itoa(int(user.ID))

		var sqlPattern string
		switch matchType {
		case "ends_with":
			sqlPattern = "%" + pattern
		case "starts_with":
			sqlPattern = pattern + "%"
		default: // includes
			sqlPattern = "%" + pattern + "%"
		}

		expr, ok := metricFieldExpr(field)
		if !ok {
			return errResponse(ctx, fiber.StatusBadRequest, "invalid field")
		}

		isFieldAttr := field != "route" && field != "remote_ip" && field != "method" && field != "path" && field != "status"

		type countRow struct {
			Value        string `json:"value"`
			CompareValue string `json:"compare_value,omitempty"`
			Status       *int   `json:"status,omitempty"`
			Count        int64  `json:"count"`
		}
		var rows []countRow

		if compareBy != "" {
			compareExpr, ok := metricFieldExpr(compareBy)
			if !ok {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid compare_by")
			}
			
			isCompareAttr := compareBy != "route" && compareBy != "remote_ip" && compareBy != "method" && compareBy != "path" && compareBy != "status"

			type compareRow struct {
				Value        string `json:"value"`
				CompareValue string `json:"compare_value"`
				Status       int    `json:"status"`
				Count        int64  `json:"count"`
			}
			var compareRows []compareRow
			q := db.Model(&dbpkg.Event{}).
				Where("user_id = ? AND created_at >= ?", userID, cutoff).
				Where(expr+" LIKE ?", sqlPattern).
				Select(expr + " AS value, " + compareExpr + " AS compare_value, status AS status, COUNT(*) AS count").
				Group("value, compare_value, status").
				Order("count DESC").
				Limit(300)
				
			if isFieldAttr {
				q = q.Where("jsonb_exists(attributes::jsonb, ?)", field)
			}
			if isCompareAttr && field != compareBy {
				q = q.Where("jsonb_exists(attributes::jsonb, ?)", compareBy)
			}

			if project != "" {
				q = q.Where("project = ?", project)
			}

			if err := q.Scan(&compareRows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query pattern counts")
			}

			rows = make([]countRow, 0, len(compareRows))
			for _, r := range compareRows {
				status := r.Status
				rows = append(rows, countRow{
					Value:        r.Value,
					CompareValue: r.CompareValue,
					Status:       &status,
					Count:        r.Count,
				})
			}
		} else {
			q := db.Model(&dbpkg.Event{}).
				Where("user_id = ? AND created_at >= ?", userID, cutoff).
				Where(expr+" LIKE ?", sqlPattern).
				Select(expr + " AS value, COUNT(*) AS count").
				Group("value").
				Order("count DESC").
				Limit(100)
				
			if isFieldAttr {
				q = q.Where("jsonb_exists(attributes::jsonb, ?)", field)
			}

			if project != "" {
				q = q.Where("project = ?", project)
			}

			if err := q.Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query pattern counts")
			}
		}

		return jsonResponse(ctx, map[string]any{"counts": rows})
	}
}
