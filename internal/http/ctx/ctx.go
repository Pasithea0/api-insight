package ctx

import (
	"github.com/gofiber/fiber/v2"

	dbpkg "apiinsight/internal/db"
)

const (
	UserKey      = "user"
	APIKeyKey    = "apiKey"
	UserTokenKey = "userToken"
)

func SetUserToken(ctx *fiber.Ctx, token string) {
	ctx.Locals(UserTokenKey, token)
}

func UserTokenFromCtx(ctx *fiber.Ctx) (string, bool) {
	v := ctx.Locals(UserTokenKey)
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func SetUser(ctx *fiber.Ctx, user any) {
	ctx.Locals(UserKey, user)
}

func UserFromCtx(ctx *fiber.Ctx) (any, bool) {
	v := ctx.Locals(UserKey)
	if v == nil {
		return nil, false
	}
	return v, true
}

func SetAPIKey(ctx *fiber.Ctx, apiKey *dbpkg.APIKey) {
	ctx.Locals(APIKeyKey, apiKey)
}

func APIKeyFromCtx(ctx *fiber.Ctx) (*dbpkg.APIKey, bool) {
	v := ctx.Locals(APIKeyKey)
	if v == nil {
		return nil, false
	}
	ak, ok := v.(*dbpkg.APIKey)
	return ak, ok
}
