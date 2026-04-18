package middleware

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
	httpsession "apiinsight/internal/http/session"
)

// AdminAuth returns middleware that loads the session user and sets it on the context.
func AdminAuth(db *gorm.DB, cfg *config.Config) func(handler fiber.Handler) fiber.Handler {
	return func(next fiber.Handler) fiber.Handler {
		return func(ctx *fiber.Ctx) error {
			cookie := ctx.Cookies(httpsession.CookieName)
			if cookie == "" {
				return ctx.Redirect("/login", fiber.StatusSeeOther)
			}
			username, ok := httpsession.Verify(cookie, ctx.Context().Time(), cfg.SessionSecret)
			if !ok {
				ctx.Cookie(&fiber.Cookie{
					Name:     httpsession.CookieName,
					Value:    "",
					Path:     "/",
					HTTPOnly: true,
					SameSite: "Lax",
					Secure:   ctx.Protocol() == "https",
					Expires:  time.Now().Add(-24 * time.Hour),
				})
				return ctx.Redirect("/login", fiber.StatusSeeOther)
			}

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

// RequireAdmin enforces that the current authenticated user is an admin.
func RequireAdmin(cfg *config.Config) func(handler fiber.Handler) fiber.Handler {
	return func(next fiber.Handler) fiber.Handler {
		return func(ctx *fiber.Ctx) error {
			v := ctx.Locals(httpctx.UserKey)
			user, ok := v.(*dbpkg.User)
			if !ok || user == nil {
				return ctx.Redirect("/login", fiber.StatusSeeOther)
			}
			if !user.IsAdmin && user.Username != cfg.AdminUser {
				return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
			}
			return next(ctx)
		}
	}
}
