package metrics

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/datatypes"

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
	// Attributes is included so the frontend can display the matched
	// value when searching by an attribute key.
	Attributes datatypes.JSONMap `json:"attributes,omitempty"`
}

// pageParams reads limit/offset, clamping limit to max.
func pageParams(ctx *fiber.Ctx, defaultLimit, max int) (limit, offset int) {
	limit = defaultLimit
	if s := ctx.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > max {
				n = max
			}
			limit = n
		}
	}
	if s := ctx.Query("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// listEvents serves recent-events, all-events and search-events, which share
// one shape: the same filters, newest first, with a limit+1 trick to report
// has_more without an expensive COUNT(*).
//
// The store is chosen by RawEvents, so these endpoints read from ClickHouse
// when the mirror is configured as the source.
func listEvents(raw *dbpkg.RawEvents, defaultLimit, maxLimit int, searchable bool) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}

		limit, offset := pageParams(ctx, defaultLimit, maxLimit)

		q := dbpkg.EventQuery{
			UserID:    scopeUserID(ctx, user),
			Project:   ctx.Query("project"),
			Status:    ctx.Query("status"),
			AttrKey:   ctx.Query("attr_key"),
			AttrValue: ctx.Query("attr_value"),
			Cutoff:    func() time.Time { c, _ := parseRange(ctx); return c }(),
			Limit:     limit + 1, // fetch one extra to detect has_more
			Offset:    offset,
		}

		if searchable {
			field := ctx.Query("field")
			pattern := ctx.Query("pattern")
			if field == "" || pattern == "" {
				return errResponse(ctx, fiber.StatusBadRequest, "missing field or pattern")
			}
			// Validate the field before it reaches any store, so an invalid
			// name is a 400 rather than a 500 from the query layer.
			if _, ok := dbpkg.PostgresFieldExpr(field); !ok {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid field")
			}
			q.Field = field
			q.Pattern = likePattern(ctx.Query("type"), pattern)
		}

		// The store override is ?store=, deliberately NOT ?source=. On the
		// export endpoint ?source= already selects *what* to export, so
		// reusing the name would make the override unusable there.
		events, source, err := raw.QueryEvents(ctx.Context(), ctx.Query("store"), q)
		if err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query events")
		}

		hasMore := len(events) > limit
		if hasMore {
			events = events[:limit]
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
				Attributes: e.Attributes,
			})
		}

		return jsonResponse(ctx, map[string]any{
			"events":   rows,
			"total":    0,
			"has_more": hasMore,
			"source":   source,
		})
	}
}

// likePattern expands a match type into a SQL LIKE pattern.
func likePattern(matchType, pattern string) string {
	switch matchType {
	case "ends_with":
		return "%" + pattern
	case "starts_with":
		return pattern + "%"
	default: // includes
		return "%" + pattern + "%"
	}
}

func RecentEvents(raw *dbpkg.RawEvents) fiber.Handler {
	return listEvents(raw, 10, 200, false)
}

func AllEvents(raw *dbpkg.RawEvents) fiber.Handler {
	return listEvents(raw, 50, 200, false)
}

func SearchEvents(raw *dbpkg.RawEvents) fiber.Handler {
	return listEvents(raw, 100, 1000, true)
}
