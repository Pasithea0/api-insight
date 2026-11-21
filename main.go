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
	dbpkg "apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
	"apiinsight/internal/http/handlers/metrics"
	appmw "apiinsight/internal/http/middleware"
	"apiinsight/internal/logger"
	"apiinsight/internal/s3store"
	"apiinsight/web"
	"net/http"
)

// eventStoreStartupHours is how many completed hours a Postgres deployment
// re-aggregates at startup. It matches the historical behaviour; the object
// store uses APP_S3_STARTUP_SCAN_HOURS instead, because there every one of
// those hours is a download.
const eventStoreStartupHours = 24

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

// startObjectStore opens the S3/B2 event store when APP_EVENT_STORE=s3.
//
// Unlike the ClickHouse mirror this does NOT degrade to Postgres when the
// bucket is unreachable: the process exits. APP_EVENT_STORE=s3 is an
// instruction about where the raw stream goes, and quietly writing it into
// the database instead is precisely how the host's disk filled up — while
// reporting a healthy start. A crash-loop with a clear log is the honest
// failure mode, and the supervisor restarts it.
func startObjectStore(cfg *config.Config, zlog zerolog.Logger, retention dbpkg.ProjectRetentionLookup) *s3store.Store {
	if !cfg.S3Enabled() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := s3store.Options{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		Bucket:          cfg.S3Bucket,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
		Prefix:          cfg.S3Prefix,
		PathStyle:       cfg.S3PathStyle,
		FlushInterval:   cfg.S3FlushInterval,
		MaxBufferEvents: cfg.S3MaxBufferEvents,
		ReadTimeout:     cfg.S3ReadTimeout,
		Retention:       retention,
	}

	store, err := s3store.Connect(ctx, opts)
	if err != nil {
		zlog.Fatal().Err(err).Str("store", opts.Describe()).
			Msg("event store: cannot reach the object store; refusing to start rather than write events to Postgres")
	}

	zlog.Info().
		Str("store", opts.Describe()).
		Str("flush_interval", cfg.S3FlushInterval.String()).
		Int("max_buffer_events", cfg.S3MaxBufferEvents).
		Msg("event store: raw events are written to the object store; the Postgres events table is not written")

	return store
}

// primeHotSource fills the in-memory partial-hour view at startup by reading
// the current hour back out of the object store.
//
// Without it a restart mid-hour would show "so far this hour" as empty until
// fresh traffic arrived, because the live view is written by the ingest path
// and starts empty. One hour of objects is a bounded, one-off download.
func primeHotSource(zlog zerolog.Logger, store *s3store.Store, hot *cache.HotSource) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	now := time.Now().UTC()
	start := now.Truncate(time.Hour)

	const primeBatch = 500
	buf := make([]dbpkg.Event, 0, primeBatch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		hot.Add(buf)
		buf = buf[:0]
	}

	total := 0
	err := store.ScanEvents(ctx, start, now, dbpkg.ScanFilter{}, func(ev dbpkg.Event) error {
		buf = append(buf, ev)
		total++
		if len(buf) >= primeBatch {
			flush()
		}
		return nil
	})
	flush()

	if err != nil {
		zlog.Warn().Err(err).Int("events", total).
			Msg("event store: could not prime the partial-hour view; the current hour will fill in as traffic arrives")
		return
	}
	zlog.Info().Int("events", total).Str("hour", start.Format(time.RFC3339)).
		Msg("event store: primed the partial-hour view from the object store")
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

	sqlDB, err := dbpkg.Connect(cfg)
	if err != nil {
		zlog.Fatal().Err(err).Msg("failed to connect database")
	}

	// How long a project's events live. The object store needs this at read
	// time (it cannot rewrite an object's expiry — see internal/s3store), and
	// the retention worker needs the longest window to know when a whole hour
	// bucket is safe to drop.
	retentionLookup := dbpkg.NewRetentionResolver(sqlDB, cfg.RetentionDays)

	// The optional object store. Opened before the workers so a bad
	// configuration fails the process here rather than after ingesting.
	objectStore := startObjectStore(cfg, zlog, retentionLookup)
	if objectStore != nil {
		defer func() {
			if err := objectStore.Close(); err != nil {
				zlog.Error().Err(err).Msg("event store: close failed")
			}
		}()
	}

	var objectRetention dbpkg.ObjectRetention
	var eventScanner dbpkg.EventScanner
	if objectStore != nil {
		objectRetention = objectStore
		eventScanner = objectStore
	}
	dbpkg.StartRetentionWorker(sqlDB, objectRetention, retentionLookup)

	startupHours := eventStoreStartupHours
	if objectStore != nil {
		startupHours = cfg.S3StartupScanHours
	}
	dbpkg.StartAggregationWorker(sqlDB, eventScanner, startupHours)

	// Retention. 0 means "keep raw events forever", which is only sensible
	// when the events live in an object store: with them in Postgres it is
	// exactly the setting that filled this host's disk before.
	if cfg.RetentionUnlimited() {
		if objectStore != nil {
			zlog.Warn().Msg("retention: UNLIMITED (APP_RETENTION_DAYS=0) — raw events are kept forever; " +
				"nothing is deleted from the object store")
		} else {
			zlog.Warn().Msg("retention: UNLIMITED (APP_RETENTION_DAYS=0) while raw events are stored in Postgres — " +
				"the events table will grow without bound on the database host")
		}
	} else {
		zlog.Info().Int("retention_days", cfg.RetentionDays).
			Bool("event_store_is_s3", objectStore != nil).
			Msg("retention configured")
	}

	// After a switch to the object store the Postgres events table is dead
	// weight. Say so rather than leaving the operator to infer it from a full
	// disk.
	if objectStore != nil {
		dbpkg.WarnOrphanedEventsTable(sqlDB)
	}

	if err := dbpkg.EnsureBootstrapAdmin(sqlDB, cfg); err != nil {
		zlog.Fatal().Err(err).Msg("failed to ensure bootstrap admin")
	}

	if cfg.InternalAPIKey != "" {
		if err := dbpkg.EnsureBootstrapAPIKey(sqlDB, cfg); err != nil {
			zlog.Warn().Err(err).Msg("failed to ensure bootstrap API key (will be created on first settings page load)")
		} else {
			zlog.Info().Msg("internal API key configured and associated with admin user")
		}
	}

	handlers.InitPrometheusMetrics()

	// Object-store mode maintains the current hour from the ingest stream,
	// because there is no events table to query for it.
	hotSource := cache.NewHotSource()

	// Start partial-hour cache (refreshes every 30s to eliminate raw-events
	// queries for the current incomplete hour on every dashboard load).
	// Which source feeds it depends on where the raw stream lives: SQL when
	// Postgres holds the events, the in-memory view when the object store does.
	var partialCache *cache.PartialHourCache
	if objectStore != nil {
		partialCache = cache.NewPartialHourCacheFromProvider(hotSource, 30*time.Second)
		go primeHotSource(zlog, objectStore, hotSource)
	} else {
		partialCache = cache.NewPartialHourCache(sqlDB, 30*time.Second)
	}
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
	if objectStore != nil {
		// The object store replaces the Postgres write entirely: with
		// APP_EVENT_STORE=s3 the events table is not written at all.
		batchWriter.SetPrimary(objectStore)
		batchWriter.SetHotAggregator(hotSource)
	}
	if chClient != nil {
		batchWriter.SetSink(chClient)
	}

	// Raw-event reads. The store that holds the stream answers; the others
	// are fallbacks and Postgres is always last, so a degraded read lands on
	// the complete copy. When the object store is the event store it is the
	// default source, and Postgres is consulted only for rows written before
	// the switch.
	//
	// The scanners map holds the stores that cannot answer with SQL. It is
	// what tells the aggregate panels (traffic, top routes, attribute and
	// pattern breakdowns) and the rollup worker to stream and accumulate
	// instead of issuing a GROUP BY.
	var chReader dbpkg.EventReader
	if chClient != nil {
		chReader = chClient
	}

	readers := map[string]dbpkg.EventReader{
		dbpkg.StorePostgres: dbpkg.NewPostgresReader(sqlDB),
	}
	if chReader != nil {
		readers[dbpkg.StoreClickHouse] = chReader
	}

	scanners := map[string]dbpkg.EventScanner{}
	defaultSource := cfg.ClickHouseReadSource
	if objectStore != nil {
		readers[dbpkg.StoreS3] = objectStore
		scanners[dbpkg.StoreS3] = objectStore
		defaultSource = dbpkg.StoreS3
	}

	// Retention edits are mirrored as well. If ClickHouse is the raw-event read
	// source, a shortened window has to be enforced there too, or the mirror
	// keeps serving rows Postgres has already dropped. The concrete client is
	// only assigned to the interface when it exists: a nil *clickhouse.Client
	// stored in a non-nil interface would panic on the first method call.
	var retentionMirror handlers.RetentionMirror
	if chClient != nil {
		retentionMirror = chClient
	}

	rawEvents := dbpkg.NewRawEvents(defaultSource, readers, scanners)
	zlog.Info().
		Str("default_source", rawEvents.DefaultSource()).
		Strs("stores", rawEvents.Sources()).
		Bool("aggregate_panels_stream", rawEvents.UsesScanner()).
		Msg("raw event reads configured")

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
	app.Post("/admin/apikeys/set-retention", adminAuth(handlers.SetAPIKeyRetention(sqlDB, cfg, retentionMirror, objectStore == nil)))

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
	app.Get("/v1/metrics/traffic", adminAuth(metrics.TrafficSeries(sqlDB, partialCache, rawEvents)))
	app.Get("/v1/metrics/error-rate", adminAuth(metrics.ErrorRateSeries(sqlDB)))
	app.Get("/v1/metrics/latency-percentiles", adminAuth(metrics.LatencyPercentilesSeries(sqlDB)))
	app.Get("/v1/metrics/avg-duration", adminAuth(metrics.AvgDuration(sqlDB, partialCache, rawEvents)))
	app.Get("/v1/metrics/attribute-keys", adminAuth(metrics.AttributeKeys(sqlDB)))
	app.Get("/v1/metrics/attribute-values", adminAuth(metrics.AttributeValues(sqlDB, rawEvents)))
	app.Get("/v1/metrics/attribute-value-counts", adminAuth(metrics.AttributeValueCounts(sqlDB, rawEvents)))
	app.Get("/v1/metrics/pattern-counts", adminAuth(metrics.PatternCounts(sqlDB, rawEvents)))
	app.Get("/v1/metrics/top-routes", adminAuth(metrics.TopRoutes(sqlDB, rawEvents)))
	app.Get("/v1/public/top-endpoints", metrics.PublicTopRoutes(sqlDB, rawEvents))
	app.Get("/v1/metrics/recent", adminAuth(metrics.RecentEvents(rawEvents)))
	app.Get("/v1/metrics/all-events", adminAuth(metrics.AllEvents(rawEvents)))
	app.Get("/v1/metrics/search-events", adminAuth(metrics.SearchEvents(rawEvents)))
	app.Get("/v1/metrics/export", adminAuth(metrics.Export(sqlDB, rawEvents)))
	app.Get("/v1/metrics/event/:id", adminAuth(handlers.EventDetail(rawEvents)))

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
