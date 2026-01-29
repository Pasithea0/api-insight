package handlers

import (
	"strconv"

	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
)

func CreateUser(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		username := string(ctx.PostArgs().Peek("username"))
		password := string(ctx.PostArgs().Peek("password"))
		isAdminStr := string(ctx.PostArgs().Peek("is_admin"))

		if username == "" || password == "" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("username and password required")
			return
		}

		isAdmin := isAdminStr == "true"

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to hash password")
			return
		}

		user := &dbpkg.User{
			Username:     username,
			PasswordHash: string(hash),
			IsAdmin:      isAdmin,
		}

		if err := db.Create(user).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("failed to create user (username may already exist)")
			return
		}

		ctx.Redirect("/users", fasthttp.StatusSeeOther)
	}
}

func ResetPassword(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		idVal := ctx.UserValue("id")
		if idVal == nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}
		idStr, ok := idVal.(string)
		if !ok {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}

		var user dbpkg.User
		if err := db.First(&user, id).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("user not found")
			return
		}

		if user.Username == cfg.AdminUser {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("cannot modify bootstrap admin user")
			return
		}

		password := string(ctx.PostArgs().Peek("password"))
		if password == "" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("password required")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to hash password")
			return
		}

		if err := db.Model(&user).Update("password_hash", string(hash)).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to update password")
			return
		}

		ctx.Redirect("/users", fasthttp.StatusSeeOther)
	}
}

func DeleteUser(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		idVal := ctx.UserValue("id")
		if idVal == nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}
		idStr, ok := idVal.(string)
		if !ok {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid user ID")
			return
		}

		var user dbpkg.User
		if err := db.First(&user, id).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("user not found")
			return
		}

		if user.Username == cfg.AdminUser {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("cannot delete bootstrap admin user")
			return
		}

		if err := db.Delete(&user).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to delete user")
			return
		}

		ctx.Redirect("/users", fasthttp.StatusSeeOther)
	}
}
