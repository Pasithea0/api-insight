package middleware

import (
	"bytes"
	"strings"

	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// BearerAuth validates Bearer tokens against API keys in the database.
func BearerAuth(db *gorm.DB) func(fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			auth := ctx.Request.Header.Peek("Authorization")
			if len(auth) == 0 {
				ctx.SetStatusCode(fasthttp.StatusUnauthorized)
				ctx.SetBodyString("missing Authorization header")
				return
			}

			const prefix = "Bearer "
			if !bytes.HasPrefix(auth, []byte(prefix)) {
				ctx.SetStatusCode(fasthttp.StatusUnauthorized)
				ctx.SetBodyString("invalid Authorization header")
				return
			}

			token := strings.TrimSpace(string(auth[len(prefix):]))
			if token == "" {
				ctx.SetStatusCode(fasthttp.StatusUnauthorized)
				ctx.SetBodyString("empty bearer token")
				return
			}

			var apiKey dbpkg.APIKey
			if err := db.Where("key = ? AND active = ?", token, true).Preload("User").First(&apiKey).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					ctx.SetStatusCode(fasthttp.StatusUnauthorized)
					ctx.SetBodyString("invalid API key")
					return
				}
				ctx.SetStatusCode(fasthttp.StatusInternalServerError)
				ctx.SetBodyString("database error")
				return
			}

			httpctx.SetUserToken(ctx, token)
			httpctx.SetAPIKey(ctx, &apiKey)
			httpctx.SetUser(ctx, &apiKey.User)
			next(ctx)
		}
	}
}
