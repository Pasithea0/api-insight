package ctx

import (
	"github.com/valyala/fasthttp"

	dbpkg "apiinsight/internal/db"
)

const (
	UserKey      = "user"
	APIKeyKey    = "apiKey"
	UserTokenKey = "userToken"
)

func SetUserToken(ctx *fasthttp.RequestCtx, token string) {
	ctx.SetUserValue(UserTokenKey, token)
}

func UserTokenFromCtx(ctx *fasthttp.RequestCtx) (string, bool) {
	v := ctx.UserValue(UserTokenKey)
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func SetUser(ctx *fasthttp.RequestCtx, user any) {
	ctx.SetUserValue(UserKey, user)
}

func UserFromCtx(ctx *fasthttp.RequestCtx) (any, bool) {
	v := ctx.UserValue(UserKey)
	if v == nil {
		return nil, false
	}
	return v, true
}

func SetAPIKey(ctx *fasthttp.RequestCtx, apiKey *dbpkg.APIKey) {
	ctx.SetUserValue(APIKeyKey, apiKey)
}

func APIKeyFromCtx(ctx *fasthttp.RequestCtx) (*dbpkg.APIKey, bool) {
	v := ctx.UserValue(APIKeyKey)
	if v == nil {
		return nil, false
	}
	ak, ok := v.(*dbpkg.APIKey)
	return ak, ok
}
