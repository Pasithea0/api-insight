package db

import "testing"

// TestNormalizeEndpointUsesRealProductionRoutes is built from routes this
// service actually stored. The four /vN/media rows were 160,890 rows in a
// single hour that collapse to 21 distinct paths once the query string is
// ignored — that factor is why the rollup tables reached 127 GB.
func TestNormalizeEndpointUsesRealProductionRoutes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "the top offender, with a client-measured duration in the query",
			in:   "/v4/media?tvdb_id=269653&season=8&episode=2&tvdb_episode_id=7928568&duration_ms=1293321",
			want: "/v4/media",
		},
		{
			name: "same endpoint, different resource",
			in:   "/v3/media?tmdb_id=71789&season=1&episode=1&duration_ms=2609648",
			want: "/v3/media",
		},
		{name: "single parameter", in: "/v2/media?tmdb_id=383498", want: "/v2/media"},
		{
			name: "flags in the query string",
			in:   "/v4/media?details=true&episode=30&mapping=official&merge_unknown=false&season=1&tvdb_id=252322",
			want: "/v4/media",
		},

		{name: "nothing to strip", in: "/health", want: "/health"},
		{name: "own endpoint", in: "/v1/metrics/traffic", want: "/v1/metrics/traffic"},

		// A full URL must not fragment the endpoint by hostname.
		{
			name: "absolute URL",
			in:   "https://api.theintrodb.org/v1/media?x=1",
			want: "/v1/media",
		},
		{name: "absolute URL without a query", in: "https://api.example.com/v1/x", want: "/v1/x"},
		{name: "host-first without a scheme", in: "api.example.com/v1/x", want: "/v1/x"},

		// A relative path must survive: only host-looking heads are rewritten,
		// so "v1/media" is not silently shortened to "/media".
		{name: "relative path is left alone", in: "v1/media", want: "v1/media"},

		{name: "fragment stripped", in: "/v1/x#section", want: "/v1/x"},
		{name: "query before fragment", in: "/v1/x?a=1#frag", want: "/v1/x"},

		{name: "trailing slash", in: "/v1/media/", want: "/v1/media"},
		{name: "repeated trailing slash", in: "/v1/media///", want: "/v1/media"},
		{name: "root stays root", in: "/", want: "/"},
		{name: "bare slash collapse", in: "///", want: "/"},

		{name: "empty query", in: "/v1/media?", want: "/v1/media"},
		{name: "whitespace is trimmed", in: "  /v1/media?a=1  ", want: "/v1/media"},

		// "" is reserved for "the client sent nothing", which callers use to
		// decide whether to fall back to another field.
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace only stays empty", in: "   ", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeEndpoint(tc.in); got != tc.want {
				t.Errorf("NormalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeEndpointCollapsesTheFragmentedRoutes is the property the rollup
// tables depend on: variants of one endpoint must normalise to one key.
func TestNormalizeEndpointCollapsesTheFragmentedRoutes(t *testing.T) {
	variants := []string{
		"/v4/media?tvdb_id=1&season=1&episode=1&duration_ms=100",
		"/v4/media?tvdb_id=2&season=9&episode=4&duration_ms=200",
		"/v4/media?tvdb_id=3&season=2&duration_ms=300",
		"/v4/media/",
		"/v4/media",
		"https://api.theintrodb.org/v4/media?tvdb_id=4",
	}

	want := "/v4/media"
	seen := map[string]bool{}
	for _, v := range variants {
		got := NormalizeEndpoint(v)
		seen[got] = true
		if got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", v, got, want)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("%d distinct endpoints from one endpoint's variants: %v", len(seen), seen)
	}
}

// TestNormalizeEndpointBoundsCardinality: a route is caller-supplied and the
// rollup is keyed on it, so an oversized path must not become an oversized key.
func TestNormalizeEndpointBoundsCardinality(t *testing.T) {
	long := "/v1/" + string(make([]byte, 4096))
	// make([]byte, n) is all NULs; replace with printable characters.
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = 'a'
	}
	long = "/v1/" + string(buf)

	got := NormalizeEndpoint(long)
	if len(got) != maxEndpointBytes {
		t.Errorf("len(NormalizeEndpoint(4099-byte path)) = %d, want %d", len(got), maxEndpointBytes)
	}
	if got[:4] != "/v1/" {
		t.Errorf("truncation lost the start of the path: %q", got[:16])
	}
}

// TestNormalizeEndpointIsIdempotent: the output is fed back in whenever an
// endpoint is stored and re-read, so normalising twice must not shift it.
func TestNormalizeEndpointIsIdempotent(t *testing.T) {
	inputs := []string{
		"/v4/media?tvdb_id=1&season=1",
		"https://api.example.com/v1/x?a=1",
		"/v1/media/",
		"/",
		"api.example.com/v1/x",
	}
	for _, in := range inputs {
		once := NormalizeEndpoint(in)
		if twice := NormalizeEndpoint(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}
