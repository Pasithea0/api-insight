package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"

	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
)

func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ai_" + base64.URLEncoding.EncodeToString(b), nil
}

func CreateAPIKey(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		name := string(ctx.PostArgs().Peek("name"))
		environment := string(ctx.PostArgs().Peek("environment"))
		retentionStr := string(ctx.PostArgs().Peek("retention_days"))

		if name == "" || environment == "" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("name and environment required")
			return
		}

		maxRetention := cfg.RetentionDays
		retentionDays := maxRetention
		if retentionStr != "" {
			if v, err := strconv.Atoi(retentionStr); err == nil && v > 0 {
				if v > maxRetention {
					retentionDays = maxRetention
				} else {
					retentionDays = v
				}
			} else {
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				ctx.SetBodyString("invalid retention_days")
				return
			}
		}

		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		key, err := generateAPIKey()
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to generate API key")
			return
		}

		apiKey := &dbpkg.APIKey{
			UserID:        user.ID,
			Name:          name,
			Environment:   environment,
			Key:           key,
			Active:        true,
			RetentionDays: retentionDays,
		}

		if err := db.Create(apiKey).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("failed to create API key (name may already exist for this user)")
			return
		}

		ctx.Redirect("/settings", fasthttp.StatusSeeOther)
	}
}

func DeleteAPIKey(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		id := string(ctx.QueryArgs().Peek("id"))
		if id == "" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("id required")
			return
		}

		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("API key not found")
			return
		}

		if apiKey.UserID != user.ID && !user.IsAdmin {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("forbidden")
			return
		}

		if cfg.InternalAPIKey != "" && apiKey.Key == cfg.InternalAPIKey {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("cannot delete internal API key")
			return
		}

		if err := db.Delete(&apiKey).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to delete API key")
			return
		}

		ctx.Redirect("/settings", fasthttp.StatusSeeOther)
	}
}

func SetActiveAPIKey(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		id := string(ctx.PostArgs().Peek("id"))
		activeStr := string(ctx.PostArgs().Peek("active"))
		if id == "" || (activeStr != "true" && activeStr != "false") {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("id and active (true|false) required")
			return
		}
		active := activeStr == "true"

		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("API key not found")
			return
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("forbidden")
			return
		}

		if err := db.Model(&apiKey).Update("active", active).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to update API key")
			return
		}
		ctx.Redirect("/settings", fasthttp.StatusSeeOther)
	}
}
