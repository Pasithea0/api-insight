package metrics

import (
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

// countRow is one row of a pattern breakdown. It is package-scoped because
// both the SQL path and the scan path build it.
type countRow struct {
	Value        string `json:"value"`
	CompareValue string `json:"compare_value,omitempty"`
	Status       *int   `json:"status,omitempty"`
	Count        int64  `json:"count"`
}

func PatternCounts(db *gorm.DB, raw *dbpkg.RawEvents) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		field := ctx.Query("field") // "route", "remote_ip", or an attribute key
		compareBy := ctx.Query("compare_by")
		pattern := ctx.Query("pattern")
		matchType := ctx.Query("type") // "includes", "ends_with", "starts_with"

		if field == "" || pattern == "" {
			return errResponse(ctx, fiber.StatusBadRequest, "missing field or pattern")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)

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

		isFieldAttr := !isVirtualField(field)

		if scanner, _ := raw.DefaultScanner(); scanner != nil {
			if compareBy != "" {
				if _, valid := metricFieldExpr(compareBy); !valid {
					return errResponse(ctx, fiber.StatusBadRequest, "invalid compare_by")
				}
			}
			rows, err := scanPatternGroups(ctx.Context(), scanner, cutoff,
				dbpkg.ScanFilter{
					UserID:  userID,
					Project: project,
					Field:   field,
					Pattern: sqlPattern,
					// jsonb_exists guards: a LIKE against a missing
					// attribute is NULL in SQL, i.e. not a match, and the
					// scan reproduces that by reporting ok=false.
					AttrExists: func() string {
						if isFieldAttr {
							return field
						}
						return ""
					}(),
				}, field, compareBy, 300)
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query pattern counts")
			}
			if compareBy == "" && len(rows) > 100 {
				rows = rows[:100]
			}
			return jsonResponse(ctx, map[string]any{"counts": rows})
		}

		if compareBy != "" {
			compareExpr, ok := metricFieldExpr(compareBy)
			if !ok {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid compare_by")
			}

			isCompareAttr := !isVirtualField(compareBy)

			type compareRow struct {
				Value        string `json:"value"`
				CompareValue string `json:"compare_value"`
				Status       int    `json:"status"`
				Count        int64  `json:"count"`
			}
			var compareRows []compareRow
			q := db.Model(&dbpkg.Event{}).
				Where("created_at >= ?", cutoff)
			q = scopeQueryUserID(q, userID)
			q = q.
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

			rows := make([]countRow, 0, len(compareRows))
			for _, r := range compareRows {
				status := r.Status
				rows = append(rows, countRow{
					Value:        r.Value,
					CompareValue: r.CompareValue,
					Status:       &status,
					Count:        r.Count,
				})
			}
			return jsonResponse(ctx, map[string]any{"counts": rows})
		}

		var rows []countRow
		q := db.Model(&dbpkg.Event{}).
			Where("created_at >= ?", cutoff)
		q = scopeQueryUserID(q, userID)
		q = q.
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

		return jsonResponse(ctx, map[string]any{"counts": rows})
	}
}

// isVirtualField reports whether a pattern-panel field is a column-backed
// name rather than an attribute key. It mirrors the check the SQL branch
// makes inline, so both branches agree on which names are attributes.
func isVirtualField(field string) bool {
	switch field {
	case "route", "remote_ip", "method", "path", "status":
		return true
	default:
		return false
	}
}
