package db

import (
	"net/url"
	"strings"
)

// maxMetricLabelBytes bounds a route label. A route is caller-supplied, so
// without a cap a single pathological request could pin a large string in the
// metrics registry for the life of the process.
const maxMetricLabelBytes = 256

// MetricRouteLabel reduces a route to a bounded value suitable for use as a
// Prometheus label.
//
// This is a metrics-side concern ONLY. It is never applied to what gets
// stored: Event.Route keeps the full request path, query string included, at
// the operator's direction, because being able to search a route by a real
// value — a title id, a season, an API key — is the point, and it is also what
// lets the dashboard show what people are actually watching rather than
// "/media".
//
// The two cardinalities are different problems, and conflating them is how a
// good fix becomes a regression. Stripping the query string from the STORED
// route was implemented and reverted at the operator's direction; this
// function exists so the metrics problem can be solved without touching
// storage at all:
//
//   - storage cardinality is a disk and index cost, paid per event, and it is
//     accepted here;
//   - label cardinality is RAM, paid for the life of the process. A Prometheus
//     counter keeps one time series per distinct label combination and never
//     releases it, so a label whose value is unique per request is unbounded
//     memory. This service reached millions of distinct routes and ~25GB RSS
//     that way, on counters that are not even scraped.
//
// So the label is bounded and the stored route is not. Dropping the query
// string, the fragment, any scheme/host, and trailing slashes is enough to
// hold the label count to the number of distinct endpoints.
//
// Returns "" only for empty input, so callers can tell "no path" from the root
// endpoint "/".
func MetricRouteLabel(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// A client may report a full URL. Its scheme and host are not part of the
	// endpoint, and leaving them in would split one endpoint by hostname.
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			s = u.Path
		}
	}

	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}

	// Host-first input with no scheme ("api.example.com/v1/media"). Only
	// rewritten when the leading segment looks like a host, so a genuine
	// relative path ("v1/media") is left alone rather than being silently
	// shortened to "/media".
	if !strings.HasPrefix(s, "/") {
		if i := strings.IndexByte(s, '/'); i > 0 {
			if head := s[:i]; strings.ContainsAny(head, ".:") {
				s = s[i:]
			}
		}
	}

	if len(s) > 1 {
		if trimmed := strings.TrimRight(s, "/"); trimmed == "" {
			s = "/"
		} else {
			s = trimmed
		}
	}

	if s == "" {
		return "/"
	}
	if len(s) > maxMetricLabelBytes {
		s = s[:maxMetricLabelBytes]
	}
	return s
}
