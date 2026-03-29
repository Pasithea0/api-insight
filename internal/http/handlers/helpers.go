package handlers

import (
	"github.com/gofiber/fiber/v2"

	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// MustUser returns the current user from context, or sends 401 and returns (nil, false).
func MustUser(ctx *fiber.Ctx) (*dbpkg.User, bool) {
	u, ok := httpctx.UserFromCtx(ctx)
	if !ok {
		ctx.Status(fiber.StatusUnauthorized).SendString("unauthorized")
		return nil, false
	}
	user, ok := u.(*dbpkg.User)
	if !ok || user == nil {
		ctx.Status(fiber.StatusUnauthorized).SendString("unauthorized")
		return nil, false
	}
	return user, true
}
