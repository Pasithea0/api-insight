package metrics

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
)

func AttributeKeys(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		project := ctx.Query("project")

		// Query from cached attribute key index
		q := db.Model(&dbpkg.AttributeKeyIndex{})
		q = scopeQueryUserID(q, userID)
		if project != "" {
			q = q.Where("project = ?", project)
		}

		type keyRow struct {
			Key string `gorm:"column:key"`
		}
		var rows []keyRow
		if err := q.Select("key").Find(&rows).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute keys")
		}

		seen := make(map[string]bool, len(rows)+5)
		keys := make([]string, 0, len(rows)+5)
		// Virtual keys always available: top-level columns plus "query",
		// which is populated at ingest with the raw query string stripped
		// from the route (e.g. "imdb_id=tt123&season=2"). It is exposed as a
		// first-class filterable field so users can search requests by
		// movie ID or any other query parameter even though routes are now
		// stored normalized (without query strings).
		for _, k := range []string{"status", "method", "route", "path", "remote_ip", "query"} {
			seen[k] = true
			keys = append(keys, k)
		}
		for _, row := range rows {
			if row.Key != "" && !seen[row.Key] {
				seen[row.Key] = true
				keys = append(keys, row.Key)
			}
		}
		return jsonResponse(ctx, map[string]any{"keys": keys})
	}
}

func AttributeValues(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		attrKey := ctx.Query("key")
		if attrKey == "" || !safeAttrKey.MatchString(attrKey) {
			return errResponse(ctx, fiber.StatusBadRequest, "invalid or missing key")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)

		sb := "SELECT DISTINCT events.attributes::jsonb ->> ? AS value FROM events WHERE events.created_at >= ?"
		args := []any{attrKey, cutoff}
		if userID != "" {
			sb += " AND events.user_id = ?"
			args = append(args, userID)
		}
		if project != "" {
			sb += " AND events.project = ?"
			args = append(args, project)
		}
		sb += " AND jsonb_exists(events.attributes::jsonb, ?)"
		args = append(args, attrKey)

		type valRow struct {
			Value string `json:"value"`
		}
		var rows []valRow
		if err := db.Raw(sb, args...).Scan(&rows).Error; err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute values")
		}

		values := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.Value != "" {
				values = append(values, row.Value)
			}
		}
		return jsonResponse(ctx, map[string]any{"values": values})
	}
}

type attributeValueCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

func AttributeValueCounts(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := handlers.MustUser(ctx)
		if !ok {
			return nil
		}
		userID := scopeUserID(ctx, user)
		attrKey := ctx.Query("key")
		if attrKey == "" {
			return errResponse(ctx, fiber.StatusBadRequest, "missing key")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)

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

		type countRow struct {
			Value string `json:"value"`
			Count int64  `json:"count"`
		}
		var rows []countRow

		// Handle virtual attributes (top-level columns)
		var column string
		var isStatus bool
		switch attrKey {
		case "remote_ip":
			column = "remote_ip"
		case "route":
			column = "route"
		case "method":
			column = "method"
		case "path":
			column = "path"
		case "status":
			isStatus = true
		}

		hasMore := false

		if column != "" {
			q := db.Model(&dbpkg.Event{}).Where("created_at >= ?", cutoff)
			q = scopeQueryUserID(q, userID)
			if project != "" {
				q = q.Where("project = ?", project)
			}

			if err := q.
				Select(column + " AS value, COUNT(*) AS count").
				Group(column).
				Order("count DESC").
				Limit(limit + 1).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query virtual attribute counts")
			}
		} else if isStatus {
			q := db.Model(&dbpkg.Event{}).Where("created_at >= ?", cutoff)
			q = scopeQueryUserID(q, userID)
			if project != "" {
				q = q.Where("project = ?", project)
			}

			if err := q.
				Select("CAST(status AS TEXT) AS value, COUNT(*) AS count").
				Group("status").
				Order("count DESC").
				Limit(limit + 1).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query virtual attribute counts")
			}
		} else {
			if !safeAttrKey.MatchString(attrKey) {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid key")
			}

			dataSQL := "SELECT events.attributes::jsonb ->> ? AS value, COUNT(*) AS count FROM events WHERE events.created_at >= ?"
			args := []any{attrKey, cutoff}
			if userID != "" {
				dataSQL += " AND events.user_id = ?"
				args = append(args, userID)
			}
			if project != "" {
				dataSQL += " AND events.project = ?"
				args = append(args, project)
			}
			dataSQL += " AND jsonb_exists(events.attributes::jsonb, ?) GROUP BY 1 ORDER BY count DESC LIMIT ? OFFSET ?"
			args = append(args, attrKey, limit+1, offset)
			if err := db.Raw(dataSQL, args...).Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute value counts")
			}
		}

		if len(rows) > limit {
			hasMore = true
			rows = rows[:limit]
		}

		counts := make([]attributeValueCount, 0, len(rows))
		for _, row := range rows {
			counts = append(counts, attributeValueCount{Value: row.Value, Count: row.Count})
		}
		
		return jsonResponse(ctx, map[string]any{"counts": counts, "total": 0, "has_more": hasMore})
	}
}
