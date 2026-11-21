package db

import (
	"net/url"
	"strings"
)

// maxEndpointBytes bounds a stored endpoint. A route is caller-supplied, and
// the rollup tables are keyed on it, so a pathological multi-kilobyte "path"
// would be paid for once per hour per distinct value. Truncation keeps the
// cardinality of a hostile client bounded; the raw path is kept in full on the
// event itself.
const maxEndpointBytes = 512

// NormalizeEndpoint reduces a request path to the endpoint it addresses.
//
// A route in an analytics service means a low-cardinality handle for an
// endpoint, the thing you group by. It is NOT the request URL. Storing the URL
// makes every request its own route, and because the rollup tables are keyed
// on (user, project, route, hour) that turns them into a copy of the event
// stream — one row per request, plus a unique index to enforce uniqueness on
// values that were never equal in the first place.
//
// That is not hypothetical: this service stored
//
//	/v4/media?tvdb_id=269653&season=8&episode=2&tvdb_episode_id=7928568&duration_ms=1293321
//
// and 160,890 such rows in a single hour collapsed to 21 distinct paths —
// a 7,661x reduction — once the query string was ignored. The 185 GB of
// rollup tables it produced was 2.6x the raw event table it summarised.
//
// What is stripped, and why:
//
//   - the query string, because a per-request parameter (a resource id, a
//     timestamp, a client-measured duration) makes every request unique, and
//     the parameters worth grouping by belong in attributes;
//   - the fragment, which is never sent to the server anyway;
//   - scheme and host, when a client reports a full URL instead of a path;
//   - trailing slashes, which are the same endpoint and must not split the
//     rollup in two.
//
// What is deliberately NOT stripped: anything else. Collapsing path segments
// (turning /v1/metrics/traffic into /v1/metrics/*) would merge genuinely
// different endpoints, and unlike the query string there is no way to tell
// which segments are identifiers and which are names. Callers that know their
// own templates should send `route` explicitly instead.
//
// Returns "" only for empty input, so callers can tell "no path was sent" from
// the root endpoint "/".
func NormalizeEndpoint(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// A client may report a full URL rather than a path. Its scheme and host
	// are not part of the endpoint, and leaving them in would split the same
	// endpoint by hostname.
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

	// Trailing slashes address the same endpoint as the bare path.
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
	if len(s) > maxEndpointBytes {
		s = s[:maxEndpointBytes]
	}
	return s
}
