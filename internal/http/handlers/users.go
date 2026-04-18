package handlers

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
)

func CreateUser(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		currentUser, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		if !currentUser.IsAdmin && currentUser.Username != cfg.AdminUser {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		username := ctx.FormValue("username")
		password := ctx.FormValue("password")
		isAdminStr := ctx.FormValue("is_admin")

		if username == "" || password == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("username and password required")
		}

		isAdmin := isAdminStr == "true"

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to hash password")
		}

		user := &dbpkg.User{
			Username:     username,
			PasswordHash: string(hash),
			IsAdmin:      isAdmin,
		}

		if err := db.Create(user).Error; err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString("failed to create user (username may already exist)")
		}

		return ctx.Redirect("/users", fiber.StatusSeeOther)
	}
}

func ResetPassword(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		currentUser, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		if !currentUser.IsAdmin && currentUser.Username != cfg.AdminUser {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		idStr := ctx.Params("id")
		if idStr == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid user ID")
		}
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid user ID")
		}

		var user dbpkg.User
		if err := db.First(&user, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("user not found")
		}

		password := ctx.FormValue("password")
		if password == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("password required")
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to hash password")
		}

		if err := db.Model(&user).Update("password_hash", string(hash)).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update password")
		}

		return ctx.Redirect("/users", fiber.StatusSeeOther)
	}
}

func DeleteUser(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		currentUser, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		if !currentUser.IsAdmin && currentUser.Username != cfg.AdminUser {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		idStr := ctx.Params("id")
		if idStr == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid user ID")
		}
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid user ID")
		}

		var user dbpkg.User
		if err := db.First(&user, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("user not found")
		}

		if user.Username == cfg.AdminUser {
			return ctx.Status(fiber.StatusForbidden).SendString("cannot delete bootstrap admin user")
		}

		if err := db.Delete(&user).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to delete user")
		}

		return ctx.Redirect("/users", fiber.StatusSeeOther)
	}
}
