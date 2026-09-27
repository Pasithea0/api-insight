package main

import (
	"context"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"

	"apiinsight/internal/cache"
	"apiinsight/internal/clickhouse"
	"apiinsight/internal/config"
	"apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
	"apiinsight/internal/http/handlers/metrics"
	appmw "apiinsight/internal/http/middleware"
	"apiinsight/internal/logger"
	"apiinsight/web"
	"net/http"
)

// startClickHouseMirror connects the optional ClickHouse mirror. It returns a
// nil client (and a no-op closer) whenever the mirror is disabled or cannot
// be reached, so callers never have to branch on configuration.
func startClickHouseMirror(cfg *config.Config, zlog zerolog.Logger) (*clickhouse.Client, func()) {
	if !cfg.ClickHouseEnabled {
		return nil, func() {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := clickhouse.Connect(ctx, cfg.ClickHouseDSN, cfg.ClickHouseRetentionDays)
	if err != nil {
		zlog.Error().Err(err).Str("dsn", redactDSN(cfg.ClickHouseDSN)).
			Msg("clickhouse: unreachable, continuing with Postgres only")
		return nil, func() {}
	}
	if err := client.EnsureSchema(ctx); err != nil {
		zlog.Error().Err(err).Msg("clickhouse: schema setup failed, continuing with Postgres only")
		_ = client.Close()
		return nil, func() {}
	}

	zlog.Info().
		Str("dsn", redactDSN(cfg.ClickHouseDSN)).
		Int("retention_days", cfg.ClickHouseRetentionDays).
		Msg("clickhouse mirror enabled (Postgres remains the system of record)")

	return client, func() {
		if err := client.Close(); err != nil {
			zlog.Error().Err(err).Msg("clickhouse: close failed")
		}
	}
}

// redactDSN strips credentials from a DSN so it can be logged. A DSN like
// clickhouse://user:secret@host:9000/db must never reach the logs verbatim.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable dsn)"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	// Configure structured logging
	if cfg.LogLevel != "" {
		logger.SetLevel(cfg.LogLevel)
	}
	if cfg.PrettyLog {
		logger.SetPrettyOutput(os.Stderr)
	}
	// Route the standard library log package through zerolog so all
	// existing log.Printf calls get structured, timestamped output.
	logger.RouteStandardLog()
	zlog := logger.Log

	sqlDB, err := db.Connect(cfg)
	if err != nil {
		zlog.Fatal().Err(err).Msg("failed to connect database")
	}

	db.StartRetentionWorker(sqlDB)
	db.StartAggregationWorker(sqlDB)

	if err := db.EnsureBootstrapAdmin(sqlDB, cfg); err != nil {
		zlog.Fatal().Err(err).Msg("failed to ensure bootstrap admin")
	}

	if cfg.InternalAPIKey != "" {
		if err := db.EnsureBootstrapAPIKey(sqlDB, cfg); err != nil {
			zlog.Warn().Err(err).Msg("failed to ensure bootstrap API key (will be created on first settings page load)")
		} else {
			zlog.Info().Msg("internal API key configured and associated with admin user")
		}
	}

	handlers.InitPrometheusMetrics()

	// Start partial-hour cache (refreshes every 30s to eliminate raw-events
	// queries for the current incomplete hour on every dashboard load).
	partialCache := cache.NewPartialHourCache(sqlDB, 30*time.Second)
	defer partialCache.Stop()

	// Optional ClickHouse mirror. Postgres remains the system of record and
	// the service runs identically without this: a failure to reach
	// ClickHouse downgrades to Postgres-only rather than blocking startup,
	// because the mirror is an optimisation, not a dependency.
	//
	// Registered before batchWriter.Stop so that, by LIFO defer ordering,
	// the writer drains its in-flight mirror writes before the sink closes.
	chClient, closeSink := startClickHouseMirror(cfg, zlog)
	defer closeSink()

	// Batch writer for async event ingestion.
	// Buffer 100 batches (up to 5000 events each = 500K events in flight).
	batchWriter := handlers.NewBatchWriter(sqlDB, 100)
	defer batchWriter.Stop()
	if chClient != nil {
		batchWriter.SetSink(chClient)
	}

	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
	})

	// Request ID middleware for tracing requests through logs.
	app.Use(requestid.New(requestid.Config{
		ContextKey: "request_id",
	}))

	// Structured request logging middleware.
	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		dur := time.Since(start)
		zlog.Info().
			Str("method", c.Method()).
			Str("path", c.Path()).
			Int("status", c.Response().StatusCode()).
			Dur("duration", dur).
			Str("request_id", c.Locals("request_id").(string)).
			Msg("request")
		return err
	})

	internalURL := "http://localhost" + cfg.ListenAddr + "/v1/events"
	if cfg.ListenAddr != "" && cfg.ListenAddr[0] != ':' {
		internalURL = "http://" + cfg.ListenAddr + "/v1/events"
	}

	app.Use(appmw.InternalReporting(cfg, internalURL))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		sqlDB, err := sqlDB.DB()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).SendString("db error")
		}
		if err := sqlDB.Ping(); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).SendString("db unreachable")
		}
		return c.SendString("ok")
	})

	app.Use("/static", filesystem.New(filesystem.Config{
		Root: http.FS(web.StaticFS()),
	}))

	app.Get("/login", handlers.LoginForm(cfg))
	app.Post("/login", handlers.LoginSubmit(sqlDB, cfg))
	app.Post("/logout", handlers.Logout())

	adminAuth := appmw.AdminAuth(sqlDB, cfg)
	requireAdmin := appmw.RequireAdmin(cfg)

	// Dashboard pages
	app.Get("/", adminAuth(handlers.Dashboard(sqlDB, cfg)))
	app.Get("/metrics", adminAuth(handlers.MetricsPage(sqlDB, cfg)))
	app.Get("/docs", adminAuth(handlers.DocsPage(sqlDB, cfg)))
	app.Get("/settings", adminAuth(handlers.SettingsPage(sqlDB, cfg)))
	app.Get("/users", adminAuth(handlers.UsersPage(sqlDB, cfg)))

	// Admin user management
	app.Post("/admin/users/create", adminAuth(requireAdmin(handlers.CreateUser(sqlDB, cfg))))
	app.Post("/admin/users/:id/reset-password", adminAuth(requireAdmin(handlers.ResetPassword(sqlDB, cfg))))
	app.Post("/admin/users/:id/delete", adminAuth(requireAdmin(handlers.DeleteUser(sqlDB, cfg))))

	// Settings
	app.Post("/settings/password", adminAuth(handlers.ChangePasswordSelf(sqlDB, cfg)))
	app.Post("/settings/display", adminAuth(handlers.UpdateDisplaySettings(sqlDB, cfg)))

	// API key management
	app.Post("/admin/apikeys/create", adminAuth(handlers.CreateAPIKey(sqlDB, cfg)))
	app.Post("/admin/apikeys/delete", adminAuth(handlers.DeleteAPIKey(sqlDB, cfg)))
	app.Post("/admin/apikeys/set-active", adminAuth(handlers.SetActiveAPIKey(sqlDB, cfg)))
	app.Post("/admin/apikeys/set-public", adminAuth(handlers.SetPublicAPIKeyState(sqlDB, cfg)))
	app.Post("/admin/apikeys/rotate-public", adminAuth(handlers.RotatePublicAPIKey(sqlDB, cfg)))

	app.Get("/admin/healthz", adminAuth(requireAdmin(func(c *fiber.Ctx) error {
		return c.SendString("admin ok")
	})))

	// Data ingestion
	app.Post("/v1/events", appmw.BearerAuth(sqlDB)(handlers.IngestHandler(sqlDB, cfg, batchWriter)))

	// Public API routes
	app.Use("/v1/public", appmw.PublicAPIKeyAuth(sqlDB)(func(c *fiber.Ctx) error {
		return c.Next()
	}))

	// Metrics API — pass cache for partial-hour optimization
	app.Get("/v1/metrics/traffic", adminAuth(metrics.TrafficSeries(sqlDB, partialCache)))
	app.Get("/v1/metrics/error-rate", adminAuth(metrics.ErrorRateSeries(sqlDB)))
	app.Get("/v1/metrics/latency-percentiles", adminAuth(metrics.LatencyPercentilesSeries(sqlDB)))
	app.Get("/v1/metrics/avg-duration", adminAuth(metrics.AvgDuration(sqlDB)))
	app.Get("/v1/metrics/attribute-keys", adminAuth(metrics.AttributeKeys(sqlDB)))
	app.Get("/v1/metrics/attribute-values", adminAuth(metrics.AttributeValues(sqlDB)))
	app.Get("/v1/metrics/attribute-value-counts", adminAuth(metrics.AttributeValueCounts(sqlDB)))
	app.Get("/v1/metrics/pattern-counts", adminAuth(metrics.PatternCounts(sqlDB)))
	app.Get("/v1/metrics/top-routes", adminAuth(metrics.TopRoutes(sqlDB)))
	app.Get("/v1/public/top-endpoints", metrics.PublicTopRoutes(sqlDB))
	app.Get("/v1/metrics/recent", adminAuth(metrics.RecentEvents(sqlDB)))
	app.Get("/v1/metrics/all-events", adminAuth(metrics.AllEvents(sqlDB)))
	app.Get("/v1/metrics/search-events", adminAuth(metrics.SearchEvents(sqlDB)))
	app.Get("/v1/metrics/export", adminAuth(metrics.Export(sqlDB)))
	app.Get("/v1/metrics/event/:id", adminAuth(handlers.EventDetail(sqlDB)))

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		zlog.Info().Str("addr", cfg.ListenAddr).Msg("apiinsight listening")
		if err := app.Listen(cfg.ListenAddr); err != nil {
			zlog.Fatal().Err(err).Msg("server error")
		}
	}()

	<-quit
	zlog.Info().Msg("shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.ShutdownWithContext(ctx); err != nil {
		zlog.Error().Err(err).Msg("shutdown error")
	}

	zlog.Info().Msg("server stopped")
}