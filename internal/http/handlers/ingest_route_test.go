package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
	httpctx "apiinsight/internal/http/ctx"
)

// Two fields, two jobs, and this file tests both.
//
// Path is the request path exactly as the client reported it, query string
// included. That is a deliberate guarantee — an earlier change replaced a
// workaround that stashed the query string in attributes["query"] with storing
// the full path itself, and this keeps that intent: nothing is reconstructed,
// reformatted or dropped, and two requests with different parameters stay
// distinguishable.
//
// Route is the endpoint the request hit, which is what every grouping uses.
// The rollup tables are keyed on it, so it must not carry a query string: with
// one, every request became its own route and route_buckets grew to 358M rows
// and 127 GB — a per-request copy of the events table, with a unique index
// enforcing uniqueness on values that were never equal.
//
// They never touch a database: events land in the BatchWriter's in-memory
// pending buffer, which we inspect directly.
var metricsOnce sync.Once

func initMetricsOnce() { metricsOnce.Do(InitPrometheusMetrics) }

func TestIngestKeepsTheRequestPathVerbatim(t *testing.T) {
	initMetricsOnce()

	// BatchWriter without the background loop so nothing is flushed to a DB.
	bw := &BatchWriter{pending: make([]dbpkg.Event, 0, MaxBatchSize)}

	app := fiber.New()
	app.Post("/ingest", func(c *fiber.Ctx) error {
		httpctx.SetAPIKey(c, &dbpkg.APIKey{UserID: 7, Name: "testproject"})
		return IngestHandler(nil, &config.Config{}, bw)(c)
	})

	body := `{"events":[
		{"path":"/v3/media?imdb_id=tt123&season=2","method":"GET","status":200,"duration_ms":12},
		{"path":"/v3/media?imdb_id=tt456&season=2","method":"GET","status":200,"duration_ms":14},
		{"path":"/v3/media","method":"GET","status":200,"duration_ms":9}
	]}`

	req := httptest.NewRequest("POST", "/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	var ack struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("bad ack JSON %q: %v", raw, err)
	}
	if ack.Count != 3 {
		t.Fatalf("accepted count = %d, want 3", ack.Count)
	}

	if len(bw.pending) != 3 {
		t.Fatalf("buffered events = %d, want 3", len(bw.pending))
	}

	wantPaths := []string{
		"/v3/media?imdb_id=tt123&season=2",
		"/v3/media?imdb_id=tt456&season=2",
		"/v3/media",
	}
	for i, want := range wantPaths {
		if got := bw.pending[i].Path; got != want {
			t.Errorf("event %d Path = %q, want the request verbatim %q", i, got, want)
		}
	}

	// The same three requests are one endpoint, so the rollup key must be one
	// value. This is the assertion the disk depended on.
	for i, e := range bw.pending {
		if e.Route != "/v3/media" {
			t.Errorf("event %d Route = %q, want the endpoint %q", i, e.Route, "/v3/media")
		}
	}

	// Distinct query strings stay distinct where it matters — on the stored
	// request — but must not fragment the aggregation key.
	distinctPaths := map[string]int{}
	for _, e := range bw.pending {
		distinctPaths[e.Path]++
	}
	if len(distinctPaths) != 3 {
		t.Errorf("distinct stored paths = %d (%v), want 3", len(distinctPaths), distinctPaths)
	}

	distinctRoutes := map[string]int{}
	for _, e := range bw.pending {
		distinctRoutes[e.Route]++
	}
	if len(distinctRoutes) != 1 {
		t.Errorf("distinct routes = %d (%v), want 1 — the query string must not "+
			"reach the rollup key", len(distinctRoutes), distinctRoutes)
	}
}

// TestIngestPrefersAnExplicitRoute: a client that already knows its own route
// template should not have to rely on the normalisation heuristic. This is the
// escape hatch for endpoints whose identifiers are path segments rather than
// parameters.
func TestIngestPrefersAnExplicitRoute(t *testing.T) {
	initMetricsOnce()
	bw := &BatchWriter{pending: make([]dbpkg.Event, 0, MaxBatchSize)}

	app := fiber.New()
	app.Post("/ingest", func(c *fiber.Ctx) error {
		httpctx.SetAPIKey(c, &dbpkg.APIKey{UserID: 7, Name: "testproject"})
		return IngestHandler(nil, &config.Config{}, bw)(c)
	})

	body := `{"events":[
		{"path":"/v4/shows/12345/episodes/678?lang=en","route":"/v4/shows/:id/episodes/:episode","duration_ms":5},
		{"path":"/v4/shows/99999/episodes/1?lang=de","route":"/v4/shows/:id/episodes/:episode","duration_ms":7}
	]}`

	req := httptest.NewRequest("POST", "/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(bw.pending) != 2 {
		t.Fatalf("buffered events = %d, want 2", len(bw.pending))
	}

	// Path identifiers are not safe to collapse automatically, so the explicit
	// route is used as given and the raw path is still preserved.
	for i, e := range bw.pending {
		if e.Route != "/v4/shows/:id/episodes/:episode" {
			t.Errorf("event %d Route = %q, want the client's template", i, e.Route)
		}
	}
	if bw.pending[0].Path != "/v4/shows/12345/episodes/678?lang=en" {
		t.Errorf("explicit route discarded the raw path: %q", bw.pending[0].Path)
	}
}

// TestIngestPreservesUserAttributes guards against the revert dropping
// caller-supplied attributes along with the removed "query" key.
func TestIngestPreservesUserAttributes(t *testing.T) {
	initMetricsOnce()
	bw := &BatchWriter{pending: make([]dbpkg.Event, 0, MaxBatchSize)}

	app := fiber.New()
	app.Post("/ingest", func(c *fiber.Ctx) error {
		httpctx.SetAPIKey(c, &dbpkg.APIKey{UserID: 7, Name: "testproject"})
		return IngestHandler(nil, &config.Config{}, bw)(c)
	})

	body := `{"events":[{"path":"/v3/x?q=1","duration_ms":1,
		"attributes":{"region":"eu-west","tier":"pro","query":"legit-user-supplied"}}]}`

	req := httptest.NewRequest("POST", "/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(bw.pending) != 1 {
		t.Fatalf("buffered events = %d, want 1", len(bw.pending))
	}
	e := bw.pending[0]
	if e.Path != "/v3/x?q=1" {
		t.Errorf("Path = %q, want the request verbatim", e.Path)
	}
	if e.Route != "/v3/x" {
		t.Errorf("Route = %q, want %q", e.Route, "/v3/x")
	}
	// Caller-supplied attributes must survive untouched — including one that
	// happens to be named "query".
	if e.Attributes["region"] != "eu-west" || e.Attributes["tier"] != "pro" {
		t.Errorf("caller attributes lost: %v", e.Attributes)
	}
	if e.Attributes["query"] != "legit-user-supplied" {
		t.Errorf("caller-supplied query attribute = %v, want untouched", e.Attributes["query"])
	}
}
