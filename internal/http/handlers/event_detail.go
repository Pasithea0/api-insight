package handlers

import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type eventDetailRow struct {
	ID         uint
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	UserID     string
	Project    string
	Route      string
	Method     string
	Status     int
	DurationMs int64
	RemoteIP   string
	Attributes []byte
}

func EventDetail(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		idStr := ctx.Params("id")
		if idStr == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}

		var row eventDetailRow
		if err := db.Raw("SELECT id, created_at, expires_at, user_id, project, route, method, status, duration_ms, remote_ip, attributes FROM events WHERE id = ?", idStr).Scan(&row).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to load event")
		}
		if row.ID == 0 {
			return ctx.Status(fiber.StatusNotFound).SendString("event not found")
		}

		if !user.IsAdmin && row.UserID != idStr {
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

		var attrs any
		if len(row.Attributes) > 0 {
			json.Unmarshal(row.Attributes, &attrs)
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
