package handlers

import (
	"bytes"

	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
	ui "apiinsight/web"
)

type LayoutData struct {
	Title            string
	Breadcrumb       string
	ActivePage       string
	PageTemplate     string
	ActiveProject    string
	Projects         []ProjectNav
	ChartMaxDays     int
	MaxRetentionDays int
	IsAdmin          bool
	Username         string
	AdminUser        string
	Users            []dbpkg.User
	APIKeys          []dbpkg.APIKey
	InternalAPIKey   string
	TimeFormat       string
	DateFormat       string
}

type ProjectNav struct {
	Name        string
	Environment string
}

func getLayoutData(ctx *fasthttp.RequestCtx, cfg *config.Config, activePage, breadcrumb, pageTemplate string) LayoutData {
	isAdmin := false
	username := ""
	timeFormat := "12"
	dateFormat := "dd-mm-yyyy"
	if u, ok := httpctx.UserFromCtx(ctx); ok {
		if user, ok := u.(*dbpkg.User); ok && user != nil {
			username = user.Username
			isAdmin = user.IsAdmin || username == cfg.AdminUser
			if user.TimeFormat != "" {
				timeFormat = user.TimeFormat
			}
			if user.DateFormat != "" {
				dateFormat = user.DateFormat
			}
		}
	}

	return LayoutData{
		Title:            breadcrumb,
		Breadcrumb:       breadcrumb,
		ActivePage:       activePage,
		PageTemplate:     pageTemplate,
		MaxRetentionDays: cfg.RetentionDays,
		IsAdmin:          isAdmin,
		Username:         username,
		AdminUser:        cfg.AdminUser,
		TimeFormat:       timeFormat,
		DateFormat:       dateFormat,
	}
}

func populateProjectsForLayout(data *LayoutData, db *gorm.DB, cfg *config.Config, ctx *fasthttp.RequestCtx, activeProject string) {
	if u, ok := httpctx.UserFromCtx(ctx); ok {
		if user, ok := u.(*dbpkg.User); ok && user != nil {
			var keys []dbpkg.APIKey
			if err := db.Where("user_id = ?", user.ID).Order("name, environment").Find(&keys).Error; err == nil {
				seen := make(map[string]bool)
				projects := make([]ProjectNav, 0, len(keys))
				allMax := 0
				perProjectMax := make(map[string]int)
				for _, k := range keys {
					eff := k.RetentionDays
					if eff <= 0 {
						eff = cfg.RetentionDays
					}
					if eff > allMax {
						allMax = eff
					}
					if eff > perProjectMax[k.Name] {
						perProjectMax[k.Name] = eff
					}
					if !seen[k.Name] {
						seen[k.Name] = true
						projects = append(projects, ProjectNav{Name: k.Name, Environment: k.Environment})
					}
				}
				data.Projects = projects
				if activeProject != "" {
					data.ChartMaxDays = perProjectMax[activeProject]
				}
				if data.ChartMaxDays == 0 {
					data.ChartMaxDays = allMax
				}
			}
		}
	}
	if data.ChartMaxDays == 0 {
		data.ChartMaxDays = cfg.RetentionDays
	}
	if data.ChartMaxDays <= 0 {
		data.ChartMaxDays = 30
	}
}

func renderLayout(ctx *fasthttp.RequestCtx, data LayoutData) {
	var buf bytes.Buffer
	if err := ui.Templates().ExecuteTemplate(&buf, "layout", data); err != nil {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetBodyString("render error")
		return
	}
	ctx.SetContentType("text/html; charset=utf-8")
	ctx.SetBody(buf.Bytes())
}

func MetricsPage(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		activeProject := string(ctx.QueryArgs().Peek("project"))
		data := getLayoutData(ctx, cfg, "metrics", "Metrics", "metrics")
		data.ActiveProject = activeProject
		populateProjectsForLayout(&data, db, cfg, ctx, activeProject)
		renderLayout(ctx, data)
	}
}

func SettingsPage(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		isSuperAdmin := user.Username == cfg.AdminUser

		var apiKeys []dbpkg.APIKey
		if err := db.Where("user_id = ?", user.ID).Order("created_at DESC").Find(&apiKeys).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to load API keys")
			return
		}

		if isSuperAdmin && cfg.InternalAPIKey != "" {
			hasInternal := false
			for _, k := range apiKeys {
				if k.Key == cfg.InternalAPIKey {
					hasInternal = true
					break
				}
			}
			if !hasInternal {
				var keyRow dbpkg.APIKey
				if err := db.Where("key = ?", cfg.InternalAPIKey).First(&keyRow).Error; err != nil {
					keyRow = dbpkg.APIKey{
						UserID:      user.ID,
						Name:        "api-insight",
						Environment: "internal",
						Key:         cfg.InternalAPIKey,
						Active:      true,
					}
					db.Create(&keyRow)
				} else if keyRow.UserID != user.ID {
					keyRow.UserID = user.ID
					db.Save(&keyRow)
				}
				apiKeys = append([]dbpkg.APIKey{keyRow}, apiKeys...)
			}
		}

		data := getLayoutData(ctx, cfg, "settings", "Settings", "settings")
		data.APIKeys = apiKeys
		data.InternalAPIKey = cfg.InternalAPIKey
		populateProjectsForLayout(&data, db, cfg, ctx, "")
		renderLayout(ctx, data)
	}
}

func UsersPage(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		isAdmin := user.IsAdmin || user.Username == cfg.AdminUser
		if !isAdmin {
			ctx.SetStatusCode(fasthttp.StatusForbidden)
			ctx.SetBodyString("forbidden")
			return
		}

		var users []dbpkg.User
		if err := db.Order("created_at DESC").Find(&users).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to load users")
			return
		}

		data := getLayoutData(ctx, cfg, "users", "Users", "users")
		data.Users = users
		populateProjectsForLayout(&data, db, cfg, ctx, "")
		renderLayout(ctx, data)
	}
}

func UpdateDisplaySettings(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		user, ok := MustUser(ctx)
		if !ok {
			return
		}
		timeFormat := string(ctx.PostArgs().Peek("time_format"))
		dateFormat := string(ctx.PostArgs().Peek("date_format"))
		if timeFormat != "12" && timeFormat != "24" {
			timeFormat = "12"
		}
		switch dateFormat {
		case "dd-mm-yyyy", "mm-dd-yyyy", "yyyy-mm-dd":
		default:
			dateFormat = "dd-mm-yyyy"
		}
		if err := db.Model(&dbpkg.User{}).Where("id = ?", user.ID).Updates(map[string]interface{}{
			"time_format": timeFormat,
			"date_format": dateFormat,
		}).Error; err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to save display settings")
			return
		}
		ctx.Redirect("/settings", fasthttp.StatusSeeOther)
	}
}

func DocsPage(db *gorm.DB, cfg *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		data := getLayoutData(ctx, cfg, "docs", "Docs", "docs")
		populateProjectsForLayout(&data, db, cfg, ctx, "")
		renderLayout(ctx, data)
	}
}

func Dashboard(db *gorm.DB, _ *config.Config) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.Redirect("/metrics", fasthttp.StatusSeeOther)
	}
}
