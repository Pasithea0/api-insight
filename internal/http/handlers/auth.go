package handlers

import (
	"bytes"
	"time"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	ui "apiinsight/web"
)

func LoginForm(_ *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		t := ui.Templates().Lookup("login.html")
		if t == nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("login template not found")
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, nil); err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("render error")
		}
		ctx.Set("Content-Type", "text/html; charset=utf-8")
		return ctx.Send(buf.Bytes())
	}
}

func LoginSubmit(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		username := ctx.FormValue("username")
		password := ctx.FormValue("password")

		var user dbpkg.User
		if err := db.Where("username = ?", username).First(&user).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return renderLoginError(ctx, "Invalid username or password.")
			}
			return ctx.Status(fiber.StatusInternalServerError).SendString("database error")
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
			return renderLoginError(ctx, "Invalid username or password.")
		}

		ctx.Cookie(&fiber.Cookie{
			Name:     "session_user",
			Value:    username,
			Path:     "/",
			HTTPOnly: true,
		})

		return ctx.Redirect("/", fiber.StatusSeeOther)
	}
}

func renderLoginError(ctx *fiber.Ctx, errMsg string) error {
	t := ui.Templates().Lookup("login.html")
	if t != nil {
		var buf bytes.Buffer
		_ = t.Execute(&buf, map[string]any{"Error": errMsg})
		ctx.Status(fiber.StatusUnauthorized)
		ctx.Set("Content-Type", "text/html; charset=utf-8")
		return ctx.Send(buf.Bytes())
	} else {
		return ctx.Status(fiber.StatusUnauthorized).SendString(errMsg)
	}
}

func Logout() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		ctx.Cookie(&fiber.Cookie{
			Name:    "session_user",
			Value:   "",
			Path:    "/",
			Expires: time.Now().Add(-24 * time.Hour),
		})
		return ctx.Redirect("/login", fiber.StatusSeeOther)
	}
}

func ChangePasswordSelf(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		current := ctx.FormValue("current_password")
		newPassword := ctx.FormValue("new_password")
		confirm := ctx.FormValue("confirm_password")

		if current == "" || newPassword == "" || confirm == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("all password fields are required")
		}
		if newPassword != confirm {
			return ctx.Status(fiber.StatusBadRequest).SendString("new passwords do not match")
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(current)); err != nil {
			return ctx.Status(fiber.StatusUnauthorized).SendString("current password is incorrect")
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to hash password")
		}

		if err := db.Model(&dbpkg.User{}).Where("id = ?", user.ID).Update("password_hash", string(hash)).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update password")
		}

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}
