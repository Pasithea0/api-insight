package db

import "testing"

// TestMetricRouteLabelBoundsTheLabel is about RAM, not storage. The stored
// route keeps its query string (see Event.Route); only the label is reduced,
// because a Prometheus counter never releases a time series.
func TestMetricRouteLabelBoundsTheLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a real production route keeps only its endpoint",
			in:   "/v4/media?tvdb_id=269653&season=8&episode=2&tvdb_episode_id=7928568&duration_ms=1293321",
			want: "/v4/media",
		},
		{
			name: "the same endpoint from a different viewer",
			in:   "/v3/media?tmdb_id=71789&season=1&episode=1&duration_ms=2609648",
			want: "/v3/media",
		},
		{name: "nothing to strip", in: "/health", want: "/health"},
		{name: "own endpoint", in: "/v1/metrics/traffic", want: "/v1/metrics/traffic"},

		// A full URL must not fragment the label by hostname.
		{name: "absolute URL", in: "https://api.theintrodb.org/v1/media?x=1", want: "/v1/media"},
		{name: "host-first without a scheme", in: "api.example.com/v1/x", want: "/v1/x"},
		// A relative path must survive: only host-looking heads are rewritten.
		{name: "relative path is left alone", in: "v1/media", want: "v1/media"},

		{name: "fragment stripped", in: "/v1/x#section", want: "/v1/x"},
		{name: "query before fragment", in: "/v1/x?a=1#frag", want: "/v1/x"},
		{name: "trailing slash", in: "/v1/media/", want: "/v1/media"},
		{name: "repeated trailing slash", in: "/v1/media///", want: "/v1/media"},
		{name: "root stays root", in: "/", want: "/"},
		{name: "bare slash collapse", in: "///", want: "/"},
		{name: "empty query", in: "/v1/media?", want: "/v1/media"},
		{name: "whitespace is trimmed", in: "  /v1/media?a=1  ", want: "/v1/media"},

		// "" is reserved for "the client sent nothing".
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace only stays empty", in: "   ", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MetricRouteLabel(tc.in); got != tc.want {
				t.Errorf("MetricRouteLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMetricRouteLabelCollapsesViewersOfOneEndpoint: the whole property the
// label bound depends on is that per-request parameters do not produce
// per-request labels.
func TestMetricRouteLabelCollapsesViewersOfOneEndpoint(t *testing.T) {
	variants := []string{
		"/v4/media?tvdb_id=1&season=1&episode=1&duration_ms=100",
		"/v4/media?tvdb_id=2&season=9&episode=4&duration_ms=200",
		"/v4/media?tvdb_id=3&season=2&duration_ms=300",
		"/v4/media/",
		"/v4/media",
		"https://api.theintrodb.org/v4/media?tvdb_id=4",
	}

	seen := map[string]bool{}
	for _, v := range variants {
		got := MetricRouteLabel(v)
		seen[got] = true
		if got != "/v4/media" {
			t.Errorf("MetricRouteLabel(%q) = %q, want %q", v, got, "/v4/media")
		}
	}
	if len(seen) != 1 {
		t.Fatalf("%d distinct labels from one endpoint's variants: %v", len(seen), seen)
	}
}

// TestMetricRouteLabelIsBounded: a caller-supplied route must not become an
// unbounded label.
func TestMetricRouteLabelIsBounded(t *testing.T) {
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = 'a'
	}
	got := MetricRouteLabel("/v1/" + string(buf))

	if len(got) != maxMetricLabelBytes {
		t.Errorf("len = %d, want %d", len(got), maxMetricLabelBytes)
	}
	if got[:4] != "/v1/" {
		t.Errorf("truncation lost the start of the path: %q", got[:16])
	}
}

// TestMetricRouteLabelIsIdempotent: the label is computed per event and may be
// recomputed from an already-reduced value.
func TestMetricRouteLabelIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"/v4/media?tvdb_id=1&season=1",
		"https://api.example.com/v1/x?a=1",
		"/v1/media/",
		"/",
		"api.example.com/v1/x",
	} {
		once := MetricRouteLabel(in)
		if twice := MetricRouteLabel(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}
