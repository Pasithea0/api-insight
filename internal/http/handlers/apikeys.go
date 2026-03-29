package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"

	"github.com/gofiber/fiber/v2"
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

func CreateAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		name := ctx.FormValue("name")
		environment := ctx.FormValue("environment")
		retentionStr := ctx.FormValue("retention_days")

		if name == "" || environment == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("name and environment required")
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
				return ctx.Status(fiber.StatusBadRequest).SendString("invalid retention_days")
			}
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		key, err := generateAPIKey()
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to generate API key")
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
			return ctx.Status(fiber.StatusBadRequest).SendString("failed to create API key (name may already exist for this user)")
		}

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func DeleteAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := ctx.Query("id")
		if id == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}

		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		if cfg.InternalAPIKey != "" && apiKey.Key == cfg.InternalAPIKey {
			return ctx.Status(fiber.StatusForbidden).SendString("cannot delete internal API key")
		}

		if err := db.Delete(&apiKey).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to delete API key")
		}

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func SetActiveAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := ctx.FormValue("id")
		activeStr := ctx.FormValue("active")
		if id == "" || (activeStr != "true" && activeStr != "false") {
			return ctx.Status(fiber.StatusBadRequest).SendString("id and active (true|false) required")
		}
		active := activeStr == "true"

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		if err := db.Model(&apiKey).Update("active", active).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update API key")
		}
		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}
