package middleware

import (
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// AdminAuth returns middleware that loads the session user and sets it on the context.
func AdminAuth(db *gorm.DB, cfg *config.Config) func(fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			cookie := ctx.Request.Header.Cookie("session_user")
			if len(cookie) == 0 {
				ctx.Redirect("/login", fasthttp.StatusSeeOther)
				return
			}
			username := string(cookie)

			var user dbpkg.User
			if err := db.Where("username = ?", username).First(&user).Error; err != nil {
				ctx.Redirect("/login", fasthttp.StatusSeeOther)
				return
			}

			if user.Username == cfg.AdminUser {
				user.IsAdmin = true
			}

			httpctx.SetUser(ctx, &user)
			next(ctx)
		}
	}
}
