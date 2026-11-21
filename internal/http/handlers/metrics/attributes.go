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
		// Virtual keys always available: top-level columns.
		for _, k := range []string{"status", "method", "route", "path", "remote_ip"} {
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

// virtualAttributeExpr maps a virtual (column-backed) attribute key onto the
// value the panel shows. ok is false for a name that is not virtual, which
// means it is an attribute key and lives in the attributes map.
//
// "path" and "route" resolve to the same column because events store the
// full request path in `route` — there is no separate path column, in
// Postgres or here.
func virtualAttributeExpr(key string) (func(dbpkg.Event) (string, bool), bool) {
	switch key {
	case "remote_ip":
		return func(ev dbpkg.Event) (string, bool) { return ev.RemoteIP, true }, true
	case "route", "path":
		return func(ev dbpkg.Event) (string, bool) { return ev.Route, true }, true
	case "method":
		return func(ev dbpkg.Event) (string, bool) { return ev.Method, true }, true
	case "status":
		return func(ev dbpkg.Event) (string, bool) { return strconv.Itoa(ev.Status), true }, true
	default:
		return nil, false
	}
}

// attributeKeyExpr renders an attribute's value the way the JSON text
// operator `->>` does, and reports ok=false when the key is absent — the
// same "no row" the SQL produces from a NULL.
func attributeKeyExpr(key string) func(dbpkg.Event) (string, bool) {
	return func(ev dbpkg.Event) (string, bool) {
		return attributeAsString(ev, key)
	}
}

func AttributeValues(db *gorm.DB, raw *dbpkg.RawEvents) fiber.Handler {
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

		if scanner, _ := raw.DefaultScanner(); scanner != nil {
			values, err := scanDistinctValues(ctx.Context(), scanner, cutoff,
				dbpkg.ScanFilter{UserID: userID, Project: project, AttrExists: attrKey},
				attributeKeyExpr(attrKey))
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute values")
			}
			return jsonResponse(ctx, map[string]any{"values": values})
		}

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

func AttributeValueCounts(db *gorm.DB, raw *dbpkg.RawEvents) fiber.Handler {
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

		// Resolve the key first: a virtual (column-backed) name and an
		// attribute name take different code paths in the SQL too.
		extract, isVirtual := virtualAttributeExpr(attrKey)
		if !isVirtual {
			if !safeAttrKey.MatchString(attrKey) {
				return errResponse(ctx, fiber.StatusBadRequest, "invalid key")
			}
			extract = attributeKeyExpr(attrKey)
		}

		filter := dbpkg.ScanFilter{UserID: userID, Project: project}
		if !isVirtual {
			filter.AttrExists = attrKey
		}

		if scanner, _ := raw.DefaultScanner(); scanner != nil {
			rows, hasMore, err := scanValueCounts(ctx.Context(), scanner, cutoff, filter, extract, limit, offset)
			if err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query attribute value counts")
			}
			counts := make([]attributeValueCount, 0, len(rows))
			for _, row := range rows {
				counts = append(counts, attributeValueCount{Value: row.Value, Count: row.Count})
			}
			return jsonResponse(ctx, map[string]any{"counts": counts, "total": 0, "has_more": hasMore})
		}

		type countRow struct {
			Value string `json:"value"`
			Count int64  `json:"count"`
		}
		var rows []countRow
		hasMore := false

		if isVirtual {
			// The virtual keys are columns, so they group directly. `path`
			// is the one exception: there is no such column, and the SQL
			// path errors on it, so it is served as `route` here.
			column := attrKey
			if column == "path" {
				column = "route"
			}
			valueExpr := column + " AS value"
			if column == "status" {
				// status is an integer column; the panel compares strings.
				valueExpr = "CAST(status AS TEXT) AS value"
			}
			q := db.Model(&dbpkg.Event{}).Where("created_at >= ?", cutoff)
			q = scopeQueryUserID(q, userID)
			if project != "" {
				q = q.Where("project = ?", project)
			}
			if err := q.
				Select(valueExpr + ", COUNT(*) AS count").
				Group(column).
				Order("count DESC").
				Limit(limit + 1).
				Offset(offset).
				Scan(&rows).Error; err != nil {
				return errResponse(ctx, fiber.StatusInternalServerError, "failed to query virtual attribute counts")
			}
		} else {
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
