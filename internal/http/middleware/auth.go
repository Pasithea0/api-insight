package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// BearerAuth validates Bearer tokens against API keys in the database.
func BearerAuth(db *gorm.DB) func(handler fiber.Handler) fiber.Handler {
	return func(next fiber.Handler) fiber.Handler {
		return func(ctx *fiber.Ctx) error {
			auth := ctx.Get("Authorization")
			if len(auth) == 0 {
				return ctx.Status(fiber.StatusUnauthorized).SendString("missing Authorization header")
			}

			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				return ctx.Status(fiber.StatusUnauthorized).SendString("invalid Authorization header")
			}

			token := strings.TrimSpace(auth[len(prefix):])
			if token == "" {
				return ctx.Status(fiber.StatusUnauthorized).SendString("empty bearer token")
			}

			var apiKey dbpkg.APIKey
			if err := db.Where("key = ? AND active = ?", token, true).Preload("User").First(&apiKey).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					return ctx.Status(fiber.StatusUnauthorized).SendString("invalid API key")
				}
				return ctx.Status(fiber.StatusInternalServerError).SendString("database error")
			}

			httpctx.SetUserToken(ctx, token)
			httpctx.SetAPIKey(ctx, &apiKey)
			httpctx.SetUser(ctx, &apiKey.User)
			return next(ctx)
		}
	}
}

// PublicAPIKeyAuth validates query parameter based public API keys for /v1/public/* routes.
func PublicAPIKeyAuth(db *gorm.DB) func(handler fiber.Handler) fiber.Handler {
	return func(next fiber.Handler) fiber.Handler {
		return func(ctx *fiber.Ctx) error {
			token := strings.TrimSpace(ctx.Query("public_key"))
			if token == "" {
				token = strings.TrimSpace(ctx.Query("key"))
			}
			if token == "" {
				return ctx.Status(fiber.StatusUnauthorized).SendString("missing public API key")
			}

			var apiKey dbpkg.APIKey
			if err := db.Where("public_key = ? AND public_enabled = ?", token, true).Preload("User").First(&apiKey).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					return ctx.Status(fiber.StatusUnauthorized).SendString("invalid public API key")
				}
				return ctx.Status(fiber.StatusInternalServerError).SendString("database error")
			}

			httpctx.SetUserToken(ctx, token)
			httpctx.SetAPIKey(ctx, &apiKey)
			httpctx.SetUser(ctx, &apiKey.User)
			return next(ctx)
		}
	}
}
