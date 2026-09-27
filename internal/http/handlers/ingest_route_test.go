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

// TestIngestStoresFullPathWithQuery verifies that the ingest handler stores
// each distinct request path verbatim in Route (query string included), so
// /v3/media?imdb_id=tt123 and /v3/media?imdb_id=tt456 are separate items.
// It never touches a database: events land in the BatchWriter's in-memory
// pending buffer, which we inspect directly.
var metricsOnce sync.Once

func initMetricsOnce() { metricsOnce.Do(InitPrometheusMetrics) }

func TestIngestStoresFullPathWithQuery(t *testing.T) {
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

	wantRoutes := []string{
		"/v3/media?imdb_id=tt123&season=2",
		"/v3/media?imdb_id=tt456&season=2",
		"/v3/media",
	}
	for i, want := range wantRoutes {
		if got := bw.pending[i].Route; got != want {
			t.Errorf("event %d Route = %q, want %q", i, got, want)
		}
	}

	// The old workaround stashed the query string in attributes["query"];
	// that must be gone now that the full path is stored in Route.
	for i, e := range bw.pending {
		if _, ok := e.Attributes["query"]; ok {
			t.Errorf("event %d still carries attributes[\"query\"] = %v", i, e.Attributes["query"])
		}
	}

	// Distinct query strings must remain distinct rows, not collapse to one route.
	seen := map[string]int{}
	for _, e := range bw.pending {
		seen[e.Route]++
	}
	if len(seen) != 3 {
		t.Errorf("distinct stored routes = %d (%v), want 3", len(seen), seen)
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
	if e.Route != "/v3/x?q=1" {
		t.Errorf("Route = %q, want %q", e.Route, "/v3/x?q=1")
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
