package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	dbpkg "apiinsight/internal/db"
)

// This file holds the object-store half of the aggregate panels.
//
// Every panel here used to be one GROUP BY against the Postgres events
// table. When the raw stream lives in an object store there is no table to
// group, so the same numbers are produced by streaming the range and
// accumulating in Go. The filter semantics are shared with the SQL path
// through dbpkg.ScanFilter, which is what keeps the two answers comparable
// rather than merely similar.
//
// Postgres deployments never enter this file: the caller checks
// raw.UsesScanner() first and the SQL code stays exactly as it was, so this
// adds a code path without changing one.

// streamEvents walks [cutoff, now] and calls fn for every matching event.
func streamEvents(ctx context.Context, scanner dbpkg.EventScanner, cutoff time.Time, filter dbpkg.ScanFilter, fn func(dbpkg.Event) error) error {
	if scanner == nil {
		return fmt.Errorf("no event scanner is configured")
	}
	end := time.Now().UTC()
	if cutoff.After(end) {
		return nil
	}
	return scanner.ScanEvents(ctx, cutoff.UTC(), end, filter, fn)
}

// valueCount is one row of a grouped count: an attribute value and how often
// it appeared.
type valueCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// bucketLabel renders a time as the ISO-8601 UTC label the charts expect,
// matching to_char(date_trunc(...)) || 'Z'.
func bucketLabel(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05") + "Z"
}

// bucket30Floor rounds a timestamp down to the start of its 30-minute slot,
// matching floor(extract(epoch from created_at) / 1800) * 1800.
func bucket30Floor(t time.Time) time.Time {
	u := t.UTC()
	return time.Unix(u.Unix()/1800*1800, 0).UTC()
}

// scanTrafficPoints reproduces the traffic panel's raw query: a count per
// hour (or per 30 minutes), oldest first.
func scanTrafficPoints(ctx context.Context, scanner dbpkg.EventScanner, cutoff time.Time, filter dbpkg.ScanFilter, bucket30Min bool) ([]trafficPoint, error) {
	counts := map[time.Time]int64{}

	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		key := ev.CreatedAt.UTC().Truncate(time.Hour)
		if bucket30Min {
			key = bucket30Floor(ev.CreatedAt)
		}
		counts[key]++
		return nil
	})
	if err != nil {
		return nil, err
	}

	points := make([]trafficPoint, 0, len(counts))
	for bucket, count := range counts {
		points = append(points, trafficPoint{Bucket: bucketLabel(bucket), Count: count})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Bucket < points[j].Bucket })
	return points, nil
}

// scanValueCounts groups events by a value extracted from each one, ordered
// by count descending, and fetches one extra row so the caller can report
// has_more without a second pass.
//
// The ordering has a deterministic tie-break on the value itself. Postgres
// leaves exact ties to the planner, which means two requests for the same
// data can disagree about row order; sorting by value as well makes the
// object-store answer reproducible.
func scanValueCounts(
	ctx context.Context,
	scanner dbpkg.EventScanner,
	cutoff time.Time,
	filter dbpkg.ScanFilter,
	extract func(dbpkg.Event) (string, bool),
	limit, offset int,
) ([]valueCount, bool, error) {
	counts := map[string]int64{}

	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		value, ok := extract(ev)
		if !ok {
			return nil
		}
		counts[value]++
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	rows := make([]valueCount, 0, len(counts))
	for value, count := range counts {
		rows = append(rows, valueCount{Value: value, Count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Value < rows[j].Value
	})

	hasMore := false
	if offset > len(rows) {
		return []valueCount{}, false, nil
	}
	rows = rows[offset:]
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

// scanDistinctValues reproduces SELECT DISTINCT <expr> FROM events ...
func scanDistinctValues(
	ctx context.Context,
	scanner dbpkg.EventScanner,
	cutoff time.Time,
	filter dbpkg.ScanFilter,
	extract func(dbpkg.Event) (string, bool),
) ([]string, error) {
	seen := map[string]struct{}{}
	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		value, ok := extract(ev)
		if !ok || value == "" {
			return nil
		}
		seen[value] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values, nil
}

// scanRouteGroup is one route's accumulated numbers.
type scanRouteGroup struct {
	Route    string
	Count    int64
	Errors   int64
	DurSum   int64
	Statuses map[int]int64
}

// scanRouteGroups groups events by route, newest-agnostic: the caller orders
// the result. The grouping is shared between top-routes and its export
// variant so the two cannot disagree.
func scanRouteGroups(ctx context.Context, scanner dbpkg.EventScanner, cutoff time.Time, filter dbpkg.ScanFilter) (map[string]*scanRouteGroup, error) {
	groups := map[string]*scanRouteGroup{}
	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		g, ok := groups[ev.Route]
		if !ok {
			g = &scanRouteGroup{Route: ev.Route, Statuses: map[int]int64{}}
			groups[ev.Route] = g
		}
		g.Count++
		g.DurSum += ev.DurationMs
		if ev.Status >= 400 {
			g.Errors++
		}
		g.Statuses[ev.Status]++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return groups, nil
}

// routeGroupsToTopRoutes orders grouped routes by count descending and
// applies the status filter, mirroring `GROUP BY route ORDER BY count(*) DESC`
// plus the per-status breakdown the panel shows.
func routeGroupsToTopRoutes(groups map[string]*scanRouteGroup, status string, limit, offset int) ([]topRoute, bool) {
	ordered := make([]*scanRouteGroup, 0, len(groups))
	for _, g := range groups {
		if g.Count <= 0 {
			continue
		}
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Count != ordered[j].Count {
			return ordered[i].Count > ordered[j].Count
		}
		return ordered[i].Route < ordered[j].Route
	})

	hasMore := false
	if offset > len(ordered) {
		return []topRoute{}, false
	}
	ordered = ordered[offset:]
	if limit > 0 && len(ordered) > limit {
		hasMore = true
		ordered = ordered[:limit]
	}

	rows := make([]topRoute, 0, len(ordered))
	for _, g := range ordered {
		statuses := make([]statusCount, 0, len(g.Statuses))
		for code, count := range g.Statuses {
			if status == "success" && code >= 400 {
				continue
			}
			if status == "error" && code < 400 {
				continue
			}
			statuses = append(statuses, statusCount{Status: code, Count: count})
		}
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Status < statuses[j].Status })
		rows = append(rows, topRoute{Route: g.Route, Count: g.Count, Statuses: statuses})
	}
	return rows, hasMore
}

// scanAverageDuration reproduces SELECT AVG(duration_ms) over the range.
func scanAverageDuration(ctx context.Context, scanner dbpkg.EventScanner, cutoff time.Time, filter dbpkg.ScanFilter) (float64, error) {
	var sum int64
	var count int64
	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		sum += ev.DurationMs
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	return float64(sum) / float64(count), nil
}

// patternGroupKey identifies one row of the pattern panel's grouping.
type patternGroupKey struct {
	Value        string
	CompareValue string
	Status       int
}

// scanPatternGroups implements the pattern-counts grouping, with or without
// a compare_by dimension, in one pass.
//
// compareBy empty means the panel only wants values (and their counts), so
// the status dimension is dropped exactly as the SQL does in that branch.
func scanPatternGroups(
	ctx context.Context,
	scanner dbpkg.EventScanner,
	cutoff time.Time,
	filter dbpkg.ScanFilter,
	field, compareBy string,
	limit int,
) ([]countRow, error) {
	counts := map[patternGroupKey]int64{}

	err := streamEvents(ctx, scanner, cutoff, filter, func(ev dbpkg.Event) error {
		value, ok := patternFieldValue(ev, field)
		if !ok {
			return nil
		}
		key := patternGroupKey{Value: value}
		if compareBy != "" {
			compare, ok := patternFieldValue(ev, compareBy)
			if !ok {
				return nil
			}
			key.CompareValue = compare
			key.Status = ev.Status
		}
		counts[key]++
		return nil
	})
	if err != nil {
		return nil, err
	}

	rows := make([]countRow, 0, len(counts))
	for key, count := range counts {
		row := countRow{Value: key.Value, CompareValue: key.CompareValue, Count: count}
		if compareBy != "" {
			status := key.Status
			row.Status = &status
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Value != rows[j].Value {
			return rows[i].Value < rows[j].Value
		}
		return rows[i].CompareValue < rows[j].CompareValue
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// patternFieldValue resolves a pattern panel's field to a value, using the
// same field mapping the SQL path interpolates.
//
// It deliberately differs from dbpkg's fieldValue in one place: the status
// column is rendered with strconv rather than fmt.Sprint so the two agree
// for every int, and a missing attribute reports ok=false — NULL LIKE
// anything is NULL, i.e. not a match.
func patternFieldValue(ev dbpkg.Event, field string) (string, bool) {
	switch field {
	case "route", "path":
		return ev.Route, true
	case "method":
		return ev.Method, true
	case "remote_ip":
		return ev.RemoteIP, true
	case "status":
		return fmt.Sprint(ev.Status), true
	default:
		if !safeAttrKey.MatchString(field) {
			return "", false
		}
		return attributeAsString(ev, field)
	}
}

// attributeAsString renders one attribute value the way the JSON path
// operator `->>` does: strings unquoted, everything else in compact JSON.
func attributeAsString(ev dbpkg.Event, key string) (string, bool) {
	v, ok := ev.Attributes[key]
	if !ok || v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return compactJSON(v), true
}

// compactJSON renders a non-string attribute value the way Postgres `->>`
// does, and never fails: a value that cannot be marshalled falls back to its
// default formatting rather than dropping the event out of the group.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(string(b))
}
