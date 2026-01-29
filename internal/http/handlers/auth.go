package handlers

import (
	"bytes"

	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	ui "apiinsight/web"
)

func LoginForm(_ *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		t := ui.Templates().Lookup("login.html")
		if t == nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("login template not found")
			return
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, nil); err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("render error")
			return
		}
		ctx.SetContentType("text/html; charset=utf-8")
		ctx.SetBody(buf.Bytes())
	}
}

func LoginSubmit(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		username := string(ctx.PostArgs().Peek("username"))
		password := string(ctx.PostArgs().Peek("password"))

		var user dbpkg.User
		if err := db.Where("username = ?", username).First(&user).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				renderLoginError(ctx, "Invalid username or password.")
				return
			}
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("database error")
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
			renderLoginError(ctx, "Invalid username or password.")
			return
		}

		var c fasthttp.Cookie
		c.SetKey("session_user")
		c.SetValue(username)
		c.SetPath("/")
		c.SetHTTPOnly(true)
		ctx.Response.Header.SetCookie(&c)

		ctx.Redirect("/", fasthttp.StatusSeeOther)
	}
}

func renderLoginError(ctx *fasthttp.RequestCtx, errMsg string) {
	t := ui.Templates().Lookup("login.html")
	if t != nil {
		var buf bytes.Buffer
		_ = t.Execute(&buf, map[string]any{"Error": errMsg})
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		ctx.SetContentType("text/html; charset=utf-8")
		ctx.SetBody(buf.Bytes())
	} else {
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		ctx.SetBodyString(errMsg)
	}
}

func Logout() fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		var c fasthttp.Cookie
		c.SetKey("session_user")
		c.SetValue("")
		c.SetPath("/")
		c.SetMaxAge(-1)
		ctx.Response.Header.SetCookie(&c)
		ctx.Redirect("/login", fasthttp.StatusSeeOther)
	}
}

func ChangePasswordSelf(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		if user.Username == cfg.AdminUser {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("cannot change password for bootstrap admin user")
			return
		}

		current := string(ctx.PostArgs().Peek("current_password"))
		newPassword := string(ctx.PostArgs().Peek("new_password"))
		confirm := string(ctx.PostArgs().Peek("confirm_password"))

		if current == "" || newPassword == "" || confirm == "" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("all password fields are required")
			return
		}
		if newPassword != confirm {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("new passwords do not match")
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(current)); err != nil {
			ctx.SetStatusCode(fasthttp.StatusUnauthorized)
			ctx.SetBodyString("current password is incorrect")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to hash password")
			return
		}

		if err := db.Model(&dbpkg.User{}).Where("id = ?", user.ID).Update("password_hash", string(hash)).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to update password")
			return
		}

		ctx.Redirect("/settings", fasthttp.StatusSeeOther)
	}
}
