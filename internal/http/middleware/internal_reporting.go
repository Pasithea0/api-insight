package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"

	"apiinsight/internal/config"
)

// InternalReporting reports metrics about this API Insight instance to itself.
// If APP_INTERNAL_API_KEY is not set, this middleware does nothing.
func InternalReporting(cfg *config.Config, ingestURL string) fiber.Handler {
	if cfg.InternalAPIKey == "" {
		return func(c *fiber.Ctx) error {
			return c.Next()
		}
	}

	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		duration := time.Since(start)

		path := string(c.Path())
		if path == "/v1/events" || path == "/v1/metrics" || path == "/metrics" || path == "/healthz" || path == "/login" {
			return err
		}

		status := c.Response().StatusCode()
		method := string(c.Method())
		remoteAddr := c.IP()

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

		return err
	}
}
