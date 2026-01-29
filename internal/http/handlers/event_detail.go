package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

func EventDetail(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		idVal := ctx.UserValue("id")
		if idVal == nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("id required")
			return
		}
		idStr, ok := idVal.(string)
		if !ok {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid id")
			return
		}
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid id")
			return
		}

		var e dbpkg.Event
		if err := db.First(&e, id).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				ctx.SetBodyString("event not found")
				return
			}
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to load event")
			return
		}

		if e.UserID != strconv.Itoa(int(user.ID)) {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("forbidden")
			return
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

		ctx.SetContentType("application/json")
		body, _ := json.Marshal(resp)
		ctx.SetBody(body)
	}
}
