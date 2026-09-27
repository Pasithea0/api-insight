package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	dbpkg "apiinsight/internal/db"
)

func EventDetail(raw *dbpkg.RawEvents) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		idStr := ctx.Params("id")
		if idStr == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}

		// The resolver checks the selected store first and falls back to the
		// other on a miss, because an event ingested before the mirror
		// existed lives only in Postgres, while one older than the raw
		// retention window may live only in the mirror.
		row, err := raw.EventByID(ctx.Context(), ctx.Query("store"), idStr)
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to load event")
		}
		if row == nil {
			return ctx.Status(fiber.StatusNotFound).SendString("event not found")
		}

		// Authorization: a non-admin may only read their own events.
		//
		// This previously read `row.UserID != idStr`, comparing the event's
		// owning user against the EVENT id. The requester's identity never
		// entered the comparison, so it denied every legitimate non-admin
		// request and granted access whenever an event's user_id happened to
		// equal its own id — an authorization decision made by coincidence.
		// Compare against the requesting user's id instead.
		if !user.IsAdmin && row.UserID != strconv.Itoa(int(user.ID)) {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		timeFormat := "12"
		dateFormat := "dd-mm-yyyy"
		if user.TimeFormat != "" {
			timeFormat = user.TimeFormat
		}
		if user.DateFormat != "" {
			dateFormat = user.DateFormat
		}
		createdAtDisplay := FormatEventDateTime(row.CreatedAt, timeFormat, dateFormat)

		// Attributes may legitimately be empty; the field is omitted-by-empty
		// rather than rendered as null when there is nothing to show.
		var attrs any
		if len(row.Attributes) > 0 {
			attrs = row.Attributes
		}

		resp := map[string]any{
			"id":                 row.ID,
			"created_at":         row.CreatedAt.Format(time.RFC3339Nano),
			"created_at_display": createdAtDisplay,
			"expires_at":         row.ExpiresAt,
			"method":             row.Method,
			"route":              row.Route,
			"status":             row.Status,
			"duration_ms":        row.DurationMs,
			"project":            row.Project,
			"user_id":            row.UserID,
			"remote_ip":          row.RemoteIP,
			"attributes":         attrs,
		}

		ctx.Set("Content-Type", "application/json")
		body, _ := json.Marshal(resp)
		return ctx.Send(body)
	}
}
