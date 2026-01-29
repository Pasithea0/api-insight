package handlers

import (
	"github.com/valyala/fasthttp"

	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// MustUser returns the current user from context, or sends 401 and returns (nil, false).
func MustUser(ctx *fasthttp.RequestCtx) (*dbpkg.User, bool) {
	u, ok := httpctx.UserFromCtx(ctx)
	if !ok {
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		ctx.SetBodyString("unauthorized")
		return nil, false
	}
	user, ok := u.(*dbpkg.User)
	if !ok || user == nil {
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		ctx.SetBodyString("unauthorized")
		return nil, false
	}
	return user, true
}
