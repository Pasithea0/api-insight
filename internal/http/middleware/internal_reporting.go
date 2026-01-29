package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/valyala/fasthttp"

	"apiinsight/internal/config"
)

// InternalReporting reports metrics about this API Insight instance to itself.
// If APP_INTERNAL_API_KEY is not set, this middleware does nothing.
func InternalReporting(cfg *config.Config, ingestURL string) func(fasthttp.RequestHandler) fasthttp.RequestHandler {
	if cfg.InternalAPIKey == "" {
		return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
			return next
		}
	}

	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			start := time.Now()
			next(ctx)
			duration := time.Since(start)

			path := string(ctx.Path())
			if path == "/v1/events" || path == "/v1/metrics" || path == "/metrics" || path == "/healthz" || path == "/login" {
				return
			}

			status := ctx.Response.StatusCode()
			method := string(ctx.Method())
			remoteAddr := ctx.RemoteAddr().String()

			go func() {
				event := map[string]interface{}{
					"timestamp":   time.Now(),
					"path":        path,
					"method":      method,
					"status":      status,
					"duration_ms": duration.Milliseconds(),
					"remote_ip":   remoteAddr,
					"attributes": map[string]interface{}{
						"env": "internal",
					},
				}
				payload := map[string]interface{}{
					"events": []interface{}{event},
				}
				body, _ := json.Marshal(payload)
				req, _ := http.NewRequest("POST", ingestURL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+cfg.InternalAPIKey)
				client := &http.Client{Timeout: 2 * time.Second}
				_, _ = client.Do(req)
			}()
		}
	}
}
