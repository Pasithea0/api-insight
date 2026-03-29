package middleware

import (
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// AdminAuth returns middleware that loads the session user and sets it on the context.
func AdminAuth(db *gorm.DB, cfg *config.Config) func(handler fiber.Handler) fiber.Handler {
	return func(next fiber.Handler) fiber.Handler {
		return func(ctx *fiber.Ctx) error {
			cookie := ctx.Cookies("session_user")
			if cookie == "" {
				return ctx.Redirect("/login", fiber.StatusSeeOther)
			}
			username := cookie

			var user dbpkg.User
			if err := db.Where("username = ?", username).First(&user).Error; err != nil {
				return ctx.Redirect("/login", fiber.StatusSeeOther)
			}

			if user.Username == cfg.AdminUser {
				user.IsAdmin = true
			}

			httpctx.SetUser(ctx, &user)
			return next(ctx)
		}
	}
}
