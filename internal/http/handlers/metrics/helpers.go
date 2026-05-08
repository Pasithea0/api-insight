package metrics

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

var safeAttrKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
var compactRange = regexp.MustCompile(`^(\d+)([hd])$`)

func parseCompactRange(raw string, now time.Time) (cutoff time.Time, bucket30Min bool, ok bool) {
	matches := compactRange.FindStringSubmatch(strings.ToLower(strings.TrimSpace(raw)))
	if len(matches) != 3 {
		return time.Time{}, false, false
	}

	value, err := strconv.Atoi(matches[1])
	if err != nil || value <= 0 {
		return time.Time{}, false, false
	}

	switch matches[2] {
	case "h":
		cutoff = now.Add(-time.Duration(value) * time.Hour)
		return cutoff, value <= 2, true
	case "d":
		cutoff = now.Add(-time.Duration(value) * 24 * time.Hour)
		return cutoff, false, true
	default:
		return time.Time{}, false, false
	}
}

// parseRange reads "hours" (float, e.g. 0.5 or 1) or "days" (int) from query and returns
// cutoff time and, for traffic, whether to use 30-min buckets (true when range <= 2 hours).
func parseRange(ctx *fiber.Ctx) (cutoff time.Time, bucket30Min bool) {
	now := time.Now()
	if raw := ctx.Query("range"); raw != "" {
		if parsedCutoff, parsedBucket30Min, ok := parseCompactRange(raw, now); ok {
			return parsedCutoff, parsedBucket30Min
		}
	}
	if h := ctx.Query("hours"); h != "" {
		if f, err := strconv.ParseFloat(h, 64); err == nil && f > 0 {
			cutoff = now.Add(-time.Duration(f * float64(time.Hour)))
			bucket30Min = f <= 2
			return cutoff, bucket30Min
		}
	}
	days := 0
	if d := ctx.Query("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			days = n
		}
	}
	if days == 0 {
		days = 1
	}
	cutoff = now.Add(-time.Duration(days) * 24 * time.Hour)
	return cutoff, false
}

func jsonResponse(ctx *fiber.Ctx, data map[string]any) error {
	return ctx.JSON(data)
}

func errResponse(ctx *fiber.Ctx, code int, msg string) error {
	return ctx.Status(code).SendString(msg)
}

func applyMetricsFilters(q *gorm.DB, status, attrKey, attrValue string) *gorm.DB {
	switch status {
	case "success":
		q = q.Where("status < ?", 400)
	case "error":
		q = q.Where("status >= ?", 400)
	}
	if attrKey != "" && attrValue != "" && safeAttrKey.MatchString(attrKey) {
		q = q.Where("attributes::jsonb ->> ? = ?", attrKey, attrValue)
	}
	return q
}

func metricFieldExpr(field string) (string, bool) {
	switch field {
	case "route", "remote_ip", "method", "path":
		return field, true
	case "status":
		return "CAST(status AS TEXT)", true
	default:
		if !safeAttrKey.MatchString(field) {
			return "", false
		}
		return "attributes::jsonb ->> '" + field + "'", true
	}
}
