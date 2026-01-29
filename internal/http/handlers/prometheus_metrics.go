package handlers

import (
	"bytes"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

func ProjectMetricsHandler(db *gorm.DB) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		apiKeyValue := string(ctx.QueryArgs().Peek("api-key"))
		if apiKeyValue == "" {
			ctx.SetStatusCode(fasthttp.StatusUnauthorized)
			ctx.SetBodyString("missing api-key query parameter")
			return
		}

		var key dbpkg.APIKey
		if err := db.Where("key = ? AND active = ?", apiKeyValue, true).Preload("User").First(&key).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				ctx.SetStatusCode(fasthttp.StatusUnauthorized)
				ctx.SetBodyString("invalid API key")
				return
			}
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("database error")
			return
		}

		projectName := key.Name

		metricFamilies, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetBodyString("failed to gather metrics")
			return
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
				ctx.SetStatusCode(fasthttp.StatusInternalServerError)
				ctx.SetBodyString("failed to encode metrics")
				return
			}
		}

		ctx.SetContentType(string(expfmt.FmtText))
		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.SetBody(buf.Bytes())
	}
}
