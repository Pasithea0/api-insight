package main

import (
	"log"

	"github.com/fasthttp/router"
	"github.com/joho/godotenv"
	"github.com/valyala/fasthttp"

	"apiinsight/internal/config"
	"apiinsight/internal/db"
	"apiinsight/internal/http/handlers"
	appmw "apiinsight/internal/http/middleware"
	ui "apiinsight/web"
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

	r := router.New()

	internalURL := "http://localhost" + cfg.ListenAddr + "/v1/events"
	if cfg.ListenAddr != "" && cfg.ListenAddr[0] != ':' {
		internalURL = "http://" + cfg.ListenAddr + "/v1/events"
	}

	// Global middleware chain: request logger, then internal reporting, then router
	handler := handlers.RequestLogger(appmw.InternalReporting(cfg, internalURL)(r.Handler))

	r.GET("/healthz", func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyString("ok")
	})

	r.ServeFS("/static/{filepath:*}", ui.StaticFS())

	r.GET("/login", handlers.LoginForm(cfg))
	r.POST("/login", handlers.LoginSubmit(sqlDB))
	r.POST("/logout", handlers.Logout())

	r.GET("/", appmw.AdminAuth(sqlDB, cfg)(handlers.Dashboard(sqlDB, cfg)))
	r.GET("/metrics", appmw.AdminAuth(sqlDB, cfg)(handlers.MetricsPage(sqlDB, cfg)))
	r.GET("/docs", appmw.AdminAuth(sqlDB, cfg)(handlers.DocsPage(sqlDB, cfg)))
	r.GET("/settings", appmw.AdminAuth(sqlDB, cfg)(handlers.SettingsPage(sqlDB, cfg)))
	r.GET("/users", appmw.AdminAuth(sqlDB, cfg)(handlers.UsersPage(sqlDB, cfg)))

	r.POST("/admin/users/create", appmw.AdminAuth(sqlDB, cfg)(handlers.CreateUser(sqlDB)))
	r.POST("/admin/users/{id}/reset-password", appmw.AdminAuth(sqlDB, cfg)(handlers.ResetPassword(sqlDB, cfg)))
	r.POST("/admin/users/{id}/delete", appmw.AdminAuth(sqlDB, cfg)(handlers.DeleteUser(sqlDB, cfg)))

	r.POST("/settings/password", appmw.AdminAuth(sqlDB, cfg)(handlers.ChangePasswordSelf(sqlDB, cfg)))
	r.POST("/settings/display", appmw.AdminAuth(sqlDB, cfg)(handlers.UpdateDisplaySettings(sqlDB, cfg)))

	r.POST("/admin/apikeys/create", appmw.AdminAuth(sqlDB, cfg)(handlers.CreateAPIKey(sqlDB, cfg)))
	r.POST("/admin/apikeys/delete", appmw.AdminAuth(sqlDB, cfg)(handlers.DeleteAPIKey(sqlDB, cfg)))
	r.POST("/admin/apikeys/set-active", appmw.AdminAuth(sqlDB, cfg)(handlers.SetActiveAPIKey(sqlDB, cfg)))

	r.GET("/admin/healthz", appmw.AdminAuth(sqlDB, cfg)(func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyString("admin ok")
	}))

	r.GET("/v1/metrics", handlers.ProjectMetricsHandler(sqlDB))
	r.POST("/v1/events", appmw.BearerAuth(sqlDB)(handlers.IngestHandler(sqlDB, cfg)))

	r.GET("/v1/metrics/traffic", appmw.AdminAuth(sqlDB, cfg)(handlers.TrafficSeries(sqlDB)))
	r.GET("/v1/metrics/error-rate", appmw.AdminAuth(sqlDB, cfg)(handlers.ErrorRateSeries(sqlDB)))
	r.GET("/v1/metrics/latency-percentiles", appmw.AdminAuth(sqlDB, cfg)(handlers.LatencyPercentilesSeries(sqlDB)))
	r.GET("/v1/metrics/avg-duration", appmw.AdminAuth(sqlDB, cfg)(handlers.AvgDuration(sqlDB)))
	r.GET("/v1/metrics/attribute-keys", appmw.AdminAuth(sqlDB, cfg)(handlers.AttributeKeys(sqlDB)))
	r.GET("/v1/metrics/attribute-values", appmw.AdminAuth(sqlDB, cfg)(handlers.AttributeValues(sqlDB)))
	r.GET("/v1/metrics/top-routes", appmw.AdminAuth(sqlDB, cfg)(handlers.TopRoutes(sqlDB)))
	r.GET("/v1/metrics/recent", appmw.AdminAuth(sqlDB, cfg)(handlers.RecentEvents(sqlDB)))
	r.GET("/v1/metrics/event/{id}", appmw.AdminAuth(sqlDB, cfg)(handlers.EventDetail(sqlDB)))

	log.Printf("apiinsight listening on %s", cfg.ListenAddr)
	if err := fasthttp.ListenAndServe(cfg.ListenAddr, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
