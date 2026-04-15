package main

import (
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/joho/godotenv"

	"apiinsight/internal/config"
	"apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
	"apiinsight/internal/http/handlers/metrics"
	appmw "apiinsight/internal/http/middleware"
	"apiinsight/web"
	"net/http"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	sqlDB, err := db.Connect(cfg)
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}

	db.StartRetentionWorker(sqlDB)
	db.StartAggregationWorker(sqlDB)

	if err := db.EnsureBootstrapAdmin(sqlDB, cfg); err != nil {
		log.Fatalf("failed to ensure bootstrap admin: %v", err)
	}

	if cfg.InternalAPIKey != "" {
		if err := db.EnsureBootstrapAPIKey(sqlDB, cfg); err != nil {
			log.Printf("warning: failed to ensure bootstrap API key: %v (will be created on first settings page load)", err)
		} else {
			log.Printf("internal API key configured and associated with admin user")
		}
	}

	handlers.InitPrometheusMetrics()

	app := fiber.New()

	internalURL := "http://localhost" + cfg.ListenAddr + "/v1/events"
	if cfg.ListenAddr != "" && cfg.ListenAddr[0] != ':' {
		internalURL = "http://" + cfg.ListenAddr + "/v1/events"
	}

	app.Use(appmw.InternalReporting(cfg, internalURL))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.Use("/static", filesystem.New(filesystem.Config{
		Root: http.FS(web.StaticFS()),
	}))

	app.Get("/login", handlers.LoginForm(cfg))
	app.Post("/login", handlers.LoginSubmit(sqlDB))
	app.Post("/logout", handlers.Logout())

	adminAuth := appmw.AdminAuth(sqlDB, cfg)
	app.Get("/", adminAuth(handlers.Dashboard(sqlDB, cfg)))
	app.Get("/metrics", adminAuth(handlers.MetricsPage(sqlDB, cfg)))
	app.Get("/docs", adminAuth(handlers.DocsPage(sqlDB, cfg)))
	app.Get("/settings", adminAuth(handlers.SettingsPage(sqlDB, cfg)))
	app.Get("/users", adminAuth(handlers.UsersPage(sqlDB, cfg)))

	app.Post("/admin/users/create", adminAuth(handlers.CreateUser(sqlDB)))
	app.Post("/admin/users/:id/reset-password", adminAuth(handlers.ResetPassword(sqlDB, cfg)))
	app.Post("/admin/users/:id/delete", adminAuth(handlers.DeleteUser(sqlDB, cfg)))

	app.Post("/settings/password", adminAuth(handlers.ChangePasswordSelf(sqlDB, cfg)))
	app.Post("/settings/display", adminAuth(handlers.UpdateDisplaySettings(sqlDB, cfg)))

	app.Post("/admin/apikeys/create", adminAuth(handlers.CreateAPIKey(sqlDB, cfg)))
	app.Post("/admin/apikeys/delete", adminAuth(handlers.DeleteAPIKey(sqlDB, cfg)))
	app.Post("/admin/apikeys/set-active", adminAuth(handlers.SetActiveAPIKey(sqlDB, cfg)))

	app.Get("/admin/healthz", adminAuth(func(c *fiber.Ctx) error {
		return c.SendString("admin ok")
	}))

	app.Get("/v1/metrics", handlers.ProjectMetricsHandler(sqlDB))
	app.Post("/v1/events", appmw.BearerAuth(sqlDB)(handlers.IngestHandler(sqlDB, cfg)))

	app.Get("/v1/metrics/traffic", adminAuth(metrics.TrafficSeries(sqlDB)))
	app.Get("/v1/metrics/error-rate", adminAuth(metrics.ErrorRateSeries(sqlDB)))
	app.Get("/v1/metrics/latency-percentiles", adminAuth(metrics.LatencyPercentilesSeries(sqlDB)))
	app.Get("/v1/metrics/avg-duration", adminAuth(metrics.AvgDuration(sqlDB)))
	app.Get("/v1/metrics/attribute-keys", adminAuth(metrics.AttributeKeys(sqlDB)))
	app.Get("/v1/metrics/attribute-values", adminAuth(metrics.AttributeValues(sqlDB)))
	app.Get("/v1/metrics/attribute-value-counts", adminAuth(metrics.AttributeValueCounts(sqlDB)))
	app.Get("/v1/metrics/pattern-counts", adminAuth(metrics.PatternCounts(sqlDB)))
	app.Get("/v1/metrics/top-routes", adminAuth(metrics.TopRoutes(sqlDB)))
	app.Get("/v1/metrics/recent", adminAuth(metrics.RecentEvents(sqlDB)))
	app.Get("/v1/metrics/all-events", adminAuth(metrics.AllEvents(sqlDB)))
	app.Get("/v1/metrics/search-events", adminAuth(metrics.SearchEvents(sqlDB)))
	app.Get("/v1/metrics/export", adminAuth(metrics.Export(sqlDB)))
	app.Get("/v1/metrics/event/:id", adminAuth(handlers.EventDetail(sqlDB)))

	log.Printf("apiinsight listening on %s", cfg.ListenAddr)
	if err := app.Listen(cfg.ListenAddr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
