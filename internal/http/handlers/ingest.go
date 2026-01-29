package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/valyala/fasthttp"
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
	Timestamp  *time.Time     `json:"timestamp,omitempty"`
	Path       string         `json:"path"`
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

func IngestHandler(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		var payload ingestRequest
		if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("invalid JSON body")
			return
		}
		if len(payload.Events) == 0 {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("no events provided")
			return
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

		records := make([]dbpkg.Event, 0, len(payload.Events))

		for _, ev := range payload.Events {
			if ev.Path == "" {
				continue
			}

			createdAt := now
			if ev.Timestamp != nil {
				createdAt = *ev.Timestamp
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
				Route:      ev.Path,
				Method:     ev.Method,
				Status:     ev.Status,
				DurationMs: ev.DurationMs,
				RemoteIP:   ev.RemoteIP,
				Attributes: attrs,
			}
			records = append(records, rec)

			labels := []string{project, ev.Path, ev.Method, strconv.Itoa(ev.Status)}
			requestsTotal.WithLabelValues(labels...).Inc()
			requestDurationBuckets.WithLabelValues(project, ev.Path, ev.Method).
				Observe(float64(ev.DurationMs) / 1000.0)
		}

		if len(records) == 0 {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBodyString("no valid events after validation")
			return
		}

		if err := db.Create(&records).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to persist events")
			return
		}

		ctx.SetStatusCode(fasthttp.StatusAccepted)
		ctx.SetContentType("application/json")
		ctx.SetBodyString(`{"status":"accepted","count":` + strconv.Itoa(len(records)) + `}`)
	}
}
