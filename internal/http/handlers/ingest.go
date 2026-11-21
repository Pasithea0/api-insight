package handlers

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

var (
	requestsTotal          *prometheus.CounterVec
	requestDurationBuckets *prometheus.HistogramVec
)

const maxIngestEventsPerRequest = 5000

func InitPrometheusMetrics() {
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "apiinsight",
			Name:      "requests_total",
			Help:      "Total number of ingested API requests.",
		},
		[]string{"project", "route", "method", "status"},
	)
	requestDurationBuckets = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "apiinsight",
			Name:      "request_duration_seconds",
			Help:      "Histogram of ingested API request durations in seconds.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		},
		[]string{"project", "route", "method"},
	)
	prometheus.MustRegister(requestsTotal, requestDurationBuckets)
}

type IngestEvent struct {
	Timestamp *time.Time `json:"timestamp,omitempty"`
	// Path is the request path, normally with its query string.
	Path string `json:"path"`
	// Route is an optional endpoint the client has already templated (for
	// example the framework's route pattern). When present it is used as-is
	// and Path is only kept for display, which is how a client that knows its
	// own routes avoids relying on the normalisation heuristic entirely.
	Route      string         `json:"route,omitempty"`
	Method     string         `json:"method,omitempty"`
	Status     int            `json:"status,omitempty"`
	UserID     string         `json:"user_id,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	RemoteIP   string         `json:"remote_ip,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type ingestRequest struct {
	Events []IngestEvent `json:"events"`
}

func IngestHandler(db *gorm.DB, cfg *config.Config, batchWriter *BatchWriter) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		var payload ingestRequest
		if err := json.Unmarshal(ctx.Body(), &payload); err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString("invalid JSON body")
		}
		if len(payload.Events) == 0 {
			return ctx.Status(fiber.StatusBadRequest).SendString("no events provided")
		}
		if len(payload.Events) > maxIngestEventsPerRequest {
			return ctx.Status(fiber.StatusRequestEntityTooLarge).
				SendString("too many events in a single request")
		}

		now := time.Now()
		retentionDays := cfg.RetentionDays
		ownerUserID := ""
		project := ""
		if ak, ok := httpctx.APIKeyFromCtx(ctx); ok && ak != nil {
			if ak.RetentionDays > 0 {
				retentionDays = ak.RetentionDays
			}
			ownerUserID = strconv.Itoa(int(ak.UserID))
			project = ak.Name
		}
		if ownerUserID == "" {
			return ctx.Status(fiber.StatusUnauthorized).SendString("missing authenticated API key")
		}

		records := make([]dbpkg.Event, 0, len(payload.Events))

		for _, ev := range payload.Events {
			// Everything that groups events — the rollup tables, the
			// Prometheus label values — keys on the route, so the route has to
			// be an endpoint rather than the request URL. See
			// dbpkg.NormalizeEndpoint: a query string in this field made every
			// request its own route.
			rawPath := strings.TrimSpace(ev.Path)
			endpoint := dbpkg.NormalizeEndpoint(ev.Route)
			if endpoint == "" {
				endpoint = dbpkg.NormalizeEndpoint(rawPath)
			}
			if endpoint == "" {
				// Neither a path nor a route: there is nothing to attribute
				// this request to.
				continue
			}
			if rawPath == "" {
				rawPath = endpoint
			}

			createdAt := now
			if ev.Timestamp != nil {
				createdAt = *ev.Timestamp
			}
			createdAt = createdAt.UTC()

			status := ev.Status
			if status < 0 {
				status = 0
			}

			durationMs := ev.DurationMs
			if durationMs < 0 {
				durationMs = 0
			}

			attrs := datatypes.JSONMap{}
			for k, v := range ev.Attributes {
				attrs[k] = v
			}

			var expiresAt *time.Time
			if retentionDays > 0 {
				t := createdAt.Add(time.Duration(retentionDays) * 24 * time.Hour)
				expiresAt = &t
			}

			rec := dbpkg.Event{
				CreatedAt:  createdAt,
				ExpiresAt:  expiresAt,
				UserID:     ownerUserID,
				Project:    project,
				Route:      endpoint,
				Path:       rawPath,
				Method:     ev.Method,
				Status:     status,
				DurationMs: durationMs,
				RemoteIP:   ev.RemoteIP,
				Attributes: attrs,
			}
			records = append(records, rec)

			// The label values are the endpoint, not the path. Prometheus
			// stores one time series per distinct label combination, so a
			// label that is unique per request grows this process's own memory
			// without bound — the same cardinality explosion that filled the
			// rollup tables, in a place that is harder to notice.
			labels := []string{project, endpoint, ev.Method, strconv.Itoa(status)}
			requestsTotal.WithLabelValues(labels...).Inc()
			requestDurationBuckets.WithLabelValues(project, endpoint, ev.Method).
				Observe(float64(durationMs) / 1000.0)
		}

		if len(records) == 0 {
			return ctx.Status(fiber.StatusBadRequest).SendString("no valid events after validation")
		}

		// Submit to batch writer — returns instantly, write happens async.
		batchWriter.Submit(records)

		ctx.Status(fiber.StatusAccepted)
		ctx.Set("Content-Type", "application/json")
		return ctx.SendString(`{"status":"accepted","count":` + strconv.Itoa(len(records)) + `}`)
	}
}
