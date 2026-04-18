package middleware

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"

	"apiinsight/internal/config"
)

const (
	internalReportingQueueSize = 1024
	internalReportingWorkers   = 2
)

// InternalReporting reports metrics about this API Insight instance to itself.
// If APP_INTERNAL_API_KEY is not set, this middleware does nothing.
func InternalReporting(cfg *config.Config, ingestURL string) fiber.Handler {
	if cfg.InternalAPIKey == "" {
		return func(c *fiber.Ctx) error {
			return c.Next()
		}
	}

	type reportJob struct {
		body []byte
	}

	queue := make(chan reportJob, internalReportingQueueSize)
	client := &http.Client{Timeout: 2 * time.Second}
	var dropped uint64

	for range internalReportingWorkers {
		go func() {
			for job := range queue {
				req, err := http.NewRequest(http.MethodPost, ingestURL, bytes.NewReader(job.body))
				if err != nil {
					log.Printf("internal reporting request build failed: %v", err)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+cfg.InternalAPIKey)
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				_ = resp.Body.Close()
			}
		}()
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
		event := map[string]any{
			"timestamp":   time.Now().UTC(),
			"path":        path,
			"method":      method,
			"status":      status,
			"duration_ms": duration.Milliseconds(),
			"remote_ip":   remoteAddr,
			"attributes": map[string]any{
				"env": "internal",
			},
		}
		payload := map[string]any{
			"events": []any{event},
		}
		body, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			log.Printf("internal reporting marshal failed: %v", marshalErr)
			return err
		}

		select {
		case queue <- reportJob{body: body}:
		default:
			dropCount := atomic.AddUint64(&dropped, 1)
			if dropCount == 1 || dropCount%100 == 0 {
				log.Printf("internal reporting queue full, dropped %d events", dropCount)
			}
		}

		return err
	}
}
