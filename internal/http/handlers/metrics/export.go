package metrics

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

func Export(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}

		source := ctx.Query("source")
		if source == "" {
			return errResponse(ctx, fiber.StatusBadRequest, "missing source")
		}

		ctx.Set("Content-Type", "text/csv")
		ctx.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_export_%s.csv\"", source, time.Now().Format("2006-01-02")))
		writer := csv.NewWriter(ctx.Response().BodyWriter())
		defer writer.Flush()

		project := ctx.Query("project")
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")
		cutoff, _ := parseRange(ctx)
		userID := strconv.Itoa(int(user.ID))

		limit := 100
		if s := ctx.Query("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 5000 {
					n = 5000
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

		switch source {
		case "recent", "all-events", "search-events":
			q := db.Model(&dbpkg.Event{}).Where("user_id = ?", userID)
			if source != "recent" {
				q = q.Where("created_at >= ?", cutoff)
			}
			if project != "" {
				q = q.Where("project = ?", project)
			}
			q = applyMetricsFilters(q, status, attrKey, attrValue)

			if source == "search-events" {
				field := ctx.Query("field")
				pattern := ctx.Query("pattern")
				matchType := ctx.Query("type")
				if field != "" && pattern != "" {
					var sqlPattern string
					switch matchType {
					case "ends_with":
						sqlPattern = "%" + pattern
					case "starts_with":
						sqlPattern = pattern + "%"
					default:
						sqlPattern = "%" + pattern + "%"
					}
					if expr, ok := metricFieldExpr(field); ok {
						q = q.Where(expr+" LIKE ?", sqlPattern)
					}
				}
			}

			var events []dbpkg.Event
			if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
				log.Printf("failed to query events for export: %v", err)
				return nil
			}

			writer.Write([]string{"time", "method", "status", "route", "duration_ms", "remote_ip", "project", "attributes"})
			for _, e := range events {
				attrs, _ := json.Marshal(e.Attributes)
				writer.Write([]string{
					e.CreatedAt.Format(time.RFC3339),
					e.Method,
					strconv.Itoa(e.Status),
					e.Route,
					strconv.FormatInt(e.DurationMs, 10),
					e.RemoteIP,
					e.Project,
					string(attrs),
				})
			}
		case "top-routes":
			q := db.Model(&dbpkg.Event{}).
				Where("user_id = ?", userID).
				Where("created_at >= ?", cutoff)
			if project != "" {
				q = q.Where("project = ?", project)
			}
			q = applyMetricsFilters(q, status, attrKey, attrValue)

			var rows []topRoute
			if err := q.
				Select("route as route, count(*) as count").
				Group("route").
				Order("count(*) DESC").
				Limit(limit).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				log.Printf("failed to query top routes for export: %v", err)
				return nil
			}

			writer.Write([]string{"route", "count"})
			for _, r := range rows {
				writer.Write([]string{r.Route, strconv.FormatInt(r.Count, 10)})
			}

		case "attribute-value-counts":
			attrKey := ctx.Query("key")
			if attrKey == "" {
				return nil // silent fail
			}

			var rows []attributeValueCount
			// This is simplified and won't handle the "virtual" attributes like remote_ip
			if !safeAttrKey.MatchString(attrKey) {
				return nil
			}

			dataSQL := "SELECT events.attributes::jsonb ->> ? AS value, COUNT(*) AS count FROM events WHERE events.user_id = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?) GROUP BY 1 ORDER BY count DESC LIMIT ? OFFSET ?"
			args := []any{attrKey, userID, cutoff, attrKey}

			if project != "" {
				dataSQL = "SELECT events.attributes::jsonb ->> ? AS value, COUNT(*) AS count FROM events WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?) GROUP BY 1 ORDER BY count DESC LIMIT ? OFFSET ?"
				args = []any{attrKey, userID, project, cutoff, attrKey}
			}

			dataArgs := append(args, limit, offset)
			if err := db.Raw(dataSQL, dataArgs...).Scan(&rows).Error; err != nil {
				log.Printf("failed to query attribute value counts for export: %v", err)
				return nil
			}

			writer.Write([]string{"value", "count"})
			for _, r := range rows {
				writer.Write([]string{r.Value, strconv.FormatInt(r.Count, 10)})
			}
		default:
		}
		return nil
	}
}
