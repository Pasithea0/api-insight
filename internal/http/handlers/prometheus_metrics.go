package handlers

import (
	"bytes"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

func ProjectMetricsHandler(db *gorm.DB) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		apiKeyValue := ctx.Query("api-key")
		if apiKeyValue == "" {
			return ctx.Status(fiber.StatusUnauthorized).SendString("missing api-key query parameter")
		}

		var key dbpkg.APIKey
		if err := db.Where("key = ? AND active = ?", apiKeyValue, true).Preload("User").First(&key).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ctx.Status(fiber.StatusUnauthorized).SendString("invalid API key")
			}
			return ctx.Status(fiber.StatusInternalServerError).SendString("database error")
		}

		projectName := key.Name

		metricFamilies, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to gather metrics")
		}

		filtered := make([]*dto.MetricFamily, 0, len(metricFamilies))
		for _, mf := range metricFamilies {
			hasProjectLabel := false
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "project" {
						hasProjectLabel = true
						break
					}
				}
				if hasProjectLabel {
					break
				}
			}

			if !hasProjectLabel {
				filtered = append(filtered, mf)
				continue
			}

			var kept []*dto.Metric
			for _, m := range mf.GetMetric() {
				include := false
				for _, l := range m.GetLabel() {
					if l.GetName() == "project" && l.GetValue() == projectName {
						include = true
						break
					}
				}
				if include {
					kept = append(kept, m)
				}
			}

			if len(kept) == 0 {
				continue
			}

			filtered = append(filtered, &dto.MetricFamily{
				Name:   mf.Name,
				Help:   mf.Help,
				Type:   mf.Type,
				Metric: kept,
			})
		}

		var buf bytes.Buffer
		encoder := expfmt.NewEncoder(&buf, expfmt.FmtText)
		for _, mf := range filtered {
			if err := encoder.Encode(mf); err != nil {
				return ctx.Status(fiber.StatusInternalServerError).SendString("failed to encode metrics")
			}
		}

		ctx.Set("Content-Type", string(expfmt.FmtText))
		ctx.Set("Cache-Control", "no-store")
		return ctx.Send(buf.Bytes())
	}
}
