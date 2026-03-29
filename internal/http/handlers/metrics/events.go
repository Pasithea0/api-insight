package metrics

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

type recentEvent struct {
	ID         uint   `json:"id"`
	Time       string `json:"time"`       // legacy, pre-formatted server time
	CreatedAt  string `json:"created_at"` // ISO 8601 UTC for client-side local formatting
	Method     string `json:"method"`
	Route      string `json:"route"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Project    string `json:"project"`
}

func RecentEvents(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		project := ctx.Query("project")
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")

		limit := 10
		if s := ctx.Query("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 200 {
					n = 200
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

		q := db.Model(&dbpkg.Event{}).Where("user_id = ?", strconv.Itoa(int(user.ID)))
		if project != "" {
			q = q.Where("project = ?", project)
		}
		q = applyMetricsFilters(q, status, attrKey, attrValue)

		var totalCount int64
		if err := q.Count(&totalCount).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to count events")
		}

		var events []dbpkg.Event
		if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query recent events")
		}

		timeFormat := "12"
		if user.TimeFormat != "" {
			timeFormat = user.TimeFormat
		}
		rows := make([]recentEvent, 0, len(events))
		for _, e := range events {
			rows = append(rows, recentEvent{
				ID:         e.ID,
				Time:       handlers.FormatEventTime(e.CreatedAt, timeFormat),
				CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
				Method:     e.Method,
				Route:      e.Route,
				Status:     e.Status,
				DurationMs: e.DurationMs,
				Project:    e.Project,
			})
		}

		hasMore := offset+limit < int(totalCount)
		return jsonResponse(ctx, map[string]any{"events": rows, "total": totalCount, "has_more": hasMore})
	}
}

func AllEvents(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		project := ctx.Query("project")
		status := ctx.Query("status")
		attrKey := ctx.Query("attr_key")
		attrValue := ctx.Query("attr_value")
		cutoff, _ := parseRange(ctx)

		limit := 50
		if s := ctx.Query("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 200 {
					n = 200
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
			Where("user_id = ?", strconv.Itoa(int(user.ID))). // Corrected line
			Where("created_at >= ?", cutoff)
		if project != "" {
			q = q.Where("project = ?", project)
		}
		q = applyMetricsFilters(q, status, attrKey, attrValue)

		var totalCount int64
		if err := q.Count(&totalCount).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to count events")
		}

		var events []dbpkg.Event
		if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query all events")
		}

		timeFormat := "12"
		if user.TimeFormat != "" {
			timeFormat = user.TimeFormat
		}
		rows := make([]recentEvent, 0, len(events))
		for _, e := range events {
			rows = append(rows, recentEvent{
				ID:         e.ID,
				Time:       handlers.FormatEventTime(e.CreatedAt, timeFormat),
				CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
				Method:     e.Method,
				Route:      e.Route,
				Status:     e.Status,
				DurationMs: e.DurationMs,
				Project:    e.Project,
			})
		}

		hasMore := offset+limit < int(totalCount)
		return jsonResponse(ctx, map[string]any{"events": rows, "total": totalCount, "has_more": hasMore})
	}
}

func SearchEvents(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		field := ctx.Query("field") // "route", "remote_ip", or an attribute key
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

		limit := 100
		if s := ctx.Query("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				if n > 1000 { // Max limit
					n = 1000
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
			Where("user_id = ? AND created_at >= ?", userID, cutoff).
			Where(expr+" LIKE ?", sqlPattern)

		if project != "" {
			q = q.Where("project = ?", project)
		}

		var totalCount int64
		if err := q.Count(&totalCount).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to count search results")
		}

		var events []dbpkg.Event
		if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to search events")
		}

		timeFormat := "12"
		if user.TimeFormat != "" {
			timeFormat = user.TimeFormat
		}
		rows := make([]recentEvent, 0, len(events))
		for _, e := range events {
			rows = append(rows, recentEvent{
				ID:         e.ID,
				Time:       handlers.FormatEventTime(e.CreatedAt, timeFormat),
				CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
				Method:     e.Method,
				Route:      e.Route,
				Status:     e.Status,
				DurationMs: e.DurationMs,
				Project:    e.Project,
			})
		}

		hasMore := offset+limit < int(totalCount)
		return jsonResponse(ctx, map[string]any{"events": rows, "total": totalCount, "has_more": hasMore})
	}
}
