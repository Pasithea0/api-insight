package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

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
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid id")
		}

		var e dbpkg.Event
		if err := db.First(&e, id).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ctx.Status(fiber.StatusNotFound).SendString("event not found")
			}
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to load event")
		}

		if !user.IsAdmin && e.UserID != strconv.Itoa(int(user.ID)) {
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
		createdAtDisplay := FormatEventDateTime(e.CreatedAt, timeFormat, dateFormat)

		resp := map[string]any{
			"id":                 e.ID,
			"created_at":         e.CreatedAt.Format(time.RFC3339Nano),
			"created_at_display": createdAtDisplay,
			"expires_at":         e.ExpiresAt,
			"method":             e.Method,
			"route":              e.Route,
			"status":             e.Status,
			"duration_ms":        e.DurationMs,
			"project":            e.Project,
			"user_id":            e.UserID,
			"remote_ip":          e.RemoteIP,
			"attributes":         e.Attributes,
		}

		ctx.Set("Content-Type", "application/json")
		body, _ := json.Marshal(resp)
		return ctx.Send(body)
	}
}
