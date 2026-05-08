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
		cutoff, _ := parseRange(ctx)

		type keyRow struct {
			Key string `json:"key"`
		}
		var rows []keyRow
		// Use jsonb_object_keys which is much faster than jsonb_each for flat maps
		err := db.Raw(
			"SELECT DISTINCT jsonb_object_keys(attributes) AS key FROM events WHERE user_id = ? AND created_at >= ?",
			strconv.Itoa(int(user.ID)), cutoff,
		).Scan(&rows).Error
		if err != nil {
			return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute keys")
		}

		keys := make([]string, 0, len(rows)+5)
		// Hard-coded searchable fields
		keys = append(keys, "status", "method", "route", "path", "remote_ip")
		for _, row := range rows {
			if row.Key != "" {
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
		attrKey := ctx.Query("key")
		if attrKey == "" || !safeAttrKey.MatchString(attrKey) {
			return errResponse(ctx, fiber.StatusBadRequest, "invalid or missing key")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)

		type valRow struct {
			Value string `json:"value"`
		}
		var rows []valRow
		if project != "" {
			err := db.Raw(
				"SELECT DISTINCT events.attributes::jsonb ->> ? AS value FROM events WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?)",
				attrKey, strconv.Itoa(int(user.ID)), project, cutoff, attrKey,
			).Scan(&rows).Error
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute values")
			}
		} else {
			err := db.Raw(
				"SELECT DISTINCT events.attributes::jsonb ->> ? AS value FROM events WHERE events.user_id = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?)",
				attrKey, strconv.Itoa(int(user.ID)), cutoff, attrKey,
			).Scan(&rows).Error
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute values")
			}
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
		attrKey := ctx.Query("key")
		if attrKey == "" {
			return errResponse(ctx, fiber.StatusBadRequest, "missing key")
		}

		project := ctx.Query("project")
		cutoff, _ := parseRange(ctx)
		userID := strconv.Itoa(int(user.ID))

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
		var totalCount int64

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

		if column != "" {
			q := db.Model(&dbpkg.Event{}).
				Where("user_id = ? AND created_at >= ?", userID, cutoff)
			if project != "" {
				q = q.Where("project = ?", project)
			}

			// Manually count distinct values for virtual attributes
			var distinctValues []string
			if err := q.Select("DISTINCT "+column).Pluck(column, &distinctValues).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to count virtual attribute values")
			}
			totalCount = int64(len(distinctValues))

			if err := q.
				Select(column + " AS value, COUNT(*) AS count").
				Group(column).
				Order("count DESC").
				Limit(limit).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query virtual attribute counts")
			}
		} else if isStatus {
			q := db.Model(&dbpkg.Event{}).
				Where("user_id = ? AND created_at >= ?", userID, cutoff)
			if project != "" {
				q = q.Where("project = ?", project)
			}

			var distinctValues []string
			if err := q.Select("DISTINCT status").Pluck("status", &distinctValues).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to count virtual attribute values")
			}
			totalCount = int64(len(distinctValues))

			if err := q.
				Select("CAST(status AS TEXT) AS value, COUNT(*) AS count").
				Group("status").
				Order("count DESC").
				Limit(limit).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query virtual attribute counts")
			}
		} else {
			if !safeAttrKey.MatchString(attrKey) {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid key")
			}

			countSQL := "SELECT COUNT(DISTINCT events.attributes::jsonb ->> ?) FROM events WHERE events.user_id = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?)"
			dataSQL := "SELECT events.attributes::jsonb ->> ? AS value, COUNT(*) AS count FROM events WHERE events.user_id = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?) GROUP BY 1 ORDER BY count DESC LIMIT ? OFFSET ?"
			args := []any{attrKey, userID, cutoff, attrKey}

			if project != "" {
				countSQL = "SELECT COUNT(DISTINCT events.attributes::jsonb ->> ?) FROM events WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?)"
				dataSQL = "SELECT events.attributes::jsonb ->> ? AS value, COUNT(*) AS count FROM events WHERE events.user_id = ? AND events.project = ? AND events.created_at >= ? AND jsonb_exists(events.attributes::jsonb, ?) GROUP BY 1 ORDER BY count DESC LIMIT ? OFFSET ?"
				args = []any{attrKey, userID, project, cutoff, attrKey}
			}

			if err := db.Raw(countSQL, args...).Scan(&totalCount).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to count attribute values")
			}

			dataArgs := append(args, limit, offset)
			if err := db.Raw(dataSQL, dataArgs...).Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute value counts")
			}
		}

		counts := make([]attributeValueCount, 0, len(rows))
		for _, row := range rows {
			counts = append(counts, attributeValueCount{Value: row.Value, Count: row.Count})
		}
		hasMore := offset+limit < int(totalCount)
		return jsonResponse(ctx, map[string]any{"counts": counts, "total": totalCount, "has_more": hasMore})
	}
}
