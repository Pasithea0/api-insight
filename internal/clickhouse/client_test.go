package clickhouse

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	dbpkg "apiinsight/internal/db"
)

// ---------------------------------------------------------------------------
// DDL unit test — no server required.
// ---------------------------------------------------------------------------

// TestEventsDDL checks the generated CREATE TABLE statement without touching a
// live server, and pins the retention behaviour (per-row TTL always present,
// global age TTL only when configured, and clamped when absurd).
func TestEventsDDL(t *testing.T) {
	t.Run("base schema with retention", func(t *testing.T) {
		ddl := eventsDDL(30)

		for _, want := range []string{
			"CREATE TABLE IF NOT EXISTS events",
			"id          UInt64",
			"created_at  DateTime64(3, 'UTC')",
			"expires_at  Nullable(DateTime64(3, 'UTC'))",
			"attributes  Map(String, String)",
			"ENGINE = MergeTree",
			"PARTITION BY toYYYYMM(created_at)",
			"ORDER BY (user_id, project, created_at, id)",
			// global age TTL, inlined as a validated literal
			"INTERVAL 30 DAY DELETE",
			// per-row TTL: rows expire on their own expires_at. ClickHouse
			// rejects a Nullable TTL expression, so expires_at is wrapped in
			// ifNull() rather than appearing bare.
			"TTL ifNull(expires_at",
			"DELETE WHERE",
		} {
			if !strings.Contains(ddl, want) {
				t.Errorf("generated DDL missing %q\n--- DDL ---\n%s", want, ddl)
			}
		}

		// Idempotent: must be IF NOT EXISTS.
		if !strings.HasPrefix(ddl, "CREATE TABLE IF NOT EXISTS") {
			t.Errorf("DDL is not idempotent: %q", firstLine(ddl))
		}

		// ZSTD codecs on the string/duration columns (user_id, project, route,
		// method, duration_ms, remote_ip).
		if got := strings.Count(ddl, "CODEC(ZSTD(3))"); got < 6 {
			t.Errorf("expected >= 6 ZSTD-coded columns, got %d\n%s", got, ddl)
		}
		if !strings.Contains(ddl, "route       String CODEC(ZSTD(3))") {
			t.Errorf("route column lacks a ZSTD codec:\n%s", ddl)
		}
		if !strings.Contains(ddl, "duration_ms Int64 CODEC(ZSTD(3))") {
			t.Errorf("duration_ms column lacks a ZSTD codec:\n%s", ddl)
		}
	})

	t.Run("per-row TTL present with retention disabled", func(t *testing.T) {
		ddl := eventsDDL(0)

		// Per-row retention is always emitted...
		if !strings.Contains(ddl, "TTL ifNull(expires_at") || !strings.Contains(ddl, "DELETE WHERE") {
			t.Errorf("per-row expires_at TTL missing when retentionDays=0:\n%s", ddl)
		}
		// ...but the global age TTL must NOT be.
		if strings.Contains(ddl, "INTERVAL") {
			t.Errorf("global age TTL emitted for retentionDays=0:\n%s", ddl)
		}
	})

	t.Run("retention is clamped, never interpolated unchecked", func(t *testing.T) {
		ddl := eventsDDL(999999)
		if !strings.Contains(ddl, "INTERVAL 3650 DAY DELETE") {
			t.Errorf("absurd retention was not clamped to 3650:\n%s", ddl)
		}
		if strings.Contains(ddl, "999999") {
			t.Errorf("unclamped retention leaked into DDL:\n%s", ddl)
		}

		// Negative / zero values disable the global TTL entirely.
		for _, n := range []int{0, -1, -1000} {
			if strings.Contains(eventsDDL(n), "INTERVAL") {
				t.Errorf("retentionDays=%d produced a global age TTL", n)
			}
		}
		if eventsDDL(-5) != eventsDDL(0) {
			t.Error("negative retention should render identically to 0")
		}
		if eventsDDL(100000) != eventsDDL(maxRetentionDays) {
			t.Error("retention above the cap should render identically to the cap")
		}
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Server-free behaviour.
// ---------------------------------------------------------------------------

// TestInsertEventsEmptyIsNoop proves the empty slice short-circuits before the
// connection is touched, so it needs no server (a zero-value Client has a nil
// connection and would panic if the guard were missing).
func TestInsertEventsEmptyIsNoop(t *testing.T) {
	c := &Client{}
	if err := c.InsertEvents(context.Background(), nil); err != nil {
		t.Fatalf("nil slice: want nil error, got %v", err)
	}
	if err := c.InsertEvents(context.Background(), []dbpkg.Event{}); err != nil {
		t.Fatalf("empty slice: want nil error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration test — requires a live ClickHouse.
// ---------------------------------------------------------------------------

// TestIntegrationRoundTrip exercises the real write path end to end: connect,
// create the schema, insert a batch, read it back over the clickhouse-go
// connection, and clean up after itself so it is re-runnable.
//
// Set CLICKHOUSE_TEST_DSN to enable it, e.g.
//
//	CLICKHOUSE_TEST_DSN='clickhouse://apiinsight:apiinsight@localhost:59000/apiinsight' \
//	    go test ./internal/clickhouse/ -v
func TestIntegrationRoundTrip(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_TEST_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_TEST_DSN is not set; skipping live ClickHouse integration test")
	}

	ctx := context.Background()

	c, err := Connect(ctx, dsn, 30)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Registered before the row cleanup below so that, with t.Cleanup's LIFO
	// ordering, the rows are deleted while the connection is still open.
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// EnsureSchema must be idempotent: calling it twice must not error.
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (second call): %v", err)
	}

	// Unique project name keys this run's rows, so cleanup can delete exactly
	// what this test inserted and the test stays re-runnable.
	project := fmt.Sprintf("ch-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteByProject(t, c, project) })

	base := time.Now().UTC().Truncate(time.Millisecond)
	expires := base.Add(24 * time.Hour)

	// Deliberately awkward attribute values: a quoted/nested payload with an
	// apostrophe, plus a non-string value that must be fmt.Sprint-ed.
	nested := `{"order":{"id":"42","note":"O'Brien \"quoted\""}}`

	events := []dbpkg.Event{
		{
			ID:         1001,
			CreatedAt:  base,
			ExpiresAt:  &expires,
			UserID:     "user-1",
			Project:    project,
			Route:      "/v1/orders",
			Method:     "GET",
			Status:     200,
			DurationMs: 12,
			RemoteIP:   "203.0.113.7",
			Attributes: map[string]any{"payload": nested, "attempts": 3, "ok": true},
		},
		{
			ID:        1002,
			CreatedAt: base.Add(10 * time.Millisecond),
			ExpiresAt: &expires,
			UserID:    "user-1",
			Project:   project,
			Route:     "/v1/orders",
			Method:    "POST",
			Status:    500,
			// deliberately no RemoteIP, no ExpiresAt? (expires set here)
			DurationMs: 250,
			RemoteIP:   "203.0.113.8",
			Attributes: map[string]any{"reason": `he said "no"`},
		},
		{
			// nil ExpiresAt -> NULL in the Nullable column, never expires.
			ID:         1003,
			CreatedAt:  base.Add(20 * time.Millisecond),
			ExpiresAt:  nil,
			UserID:     "user-1",
			Project:    project,
			Route:      "/v1/orders/{id}",
			Method:     "DELETE",
			Status:     404,
			DurationMs: 5,
			RemoteIP:   "203.0.113.9",
			Attributes: nil, // nil map -> empty map column
		},
	}

	if err := c.InsertEvents(ctx, events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	rows, err := c.conn.Query(ctx,
		`SELECT id, created_at, route, status, duration_ms, attributes, expires_at IS NULL
		 FROM events WHERE project = ? ORDER BY id`, project)
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	defer rows.Close()

	type got struct {
		id         uint64
		createdAt  time.Time
		route      string
		status     int32
		durationMs int64
		attrs      map[string]string
		noExpiry   bool
	}

	var gotRows []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.id, &g.createdAt, &g.route, &g.status, &g.durationMs, &g.attrs, &g.noExpiry); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		gotRows = append(gotRows, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	if len(gotRows) != len(events) {
		t.Fatalf("round-trip count: want %d rows, got %d (%+v)", len(events), len(gotRows), gotRows)
	}

	wantRoute := map[uint64]string{1001: "/v1/orders", 1002: "/v1/orders", 1003: "/v1/orders/{id}"}
	wantStatus := map[uint64]int32{1001: 200, 1002: 500, 1003: 404}
	wantDur := map[uint64]int64{1001: 12, 1002: 250, 1003: 5}

	for i, g := range gotRows {
		src := events[i]

		if g.id != uint64(src.ID) {
			t.Errorf("row %d: id = %d, want %d", i, g.id, src.ID)
			continue
		}
		if g.route != wantRoute[g.id] {
			t.Errorf("row %d: route = %q, want %q", i, g.route, wantRoute[g.id])
		}
		if g.status != wantStatus[g.id] {
			t.Errorf("row %d: status = %d, want %d", i, g.status, wantStatus[g.id])
		}
		if g.durationMs != wantDur[g.id] {
			t.Errorf("row %d: duration_ms = %d, want %d", i, g.durationMs, wantDur[g.id])
		}

		// created_at must round-trip to within a second (DateTime64(3) is
		// millisecond precision, so this is generous).
		if delta := g.createdAt.UTC().Sub(src.CreatedAt.UTC()); delta > time.Second || delta < -time.Second {
			t.Errorf("row %d: created_at off by %s (got %s, want %s)",
				i, delta, g.createdAt.UTC(), src.CreatedAt.UTC())
		}

		// Expiry: nil ExpiresAt must land as NULL; a set one must not.
		if wantNull := src.ExpiresAt == nil; g.noExpiry != wantNull {
			t.Errorf("row %d: expires_at IS NULL = %v, want %v", i, g.noExpiry, wantNull)
		}
	}

	// Attribute contents, including the quoted/nested value and the
	// fmt.Sprint-ed non-string values.
	attrs0 := gotRows[0].attrs
	for k, want := range map[string]string{
		"payload":  nested,
		"attempts": "3",
		"ok":       "true",
	} {
		if attrs0[k] != want {
			t.Errorf("row 1001 attributes[%q] = %q, want %q", k, attrs0[k], want)
		}
	}
	if got := gotRows[1].attrs["reason"]; got != `he said "no"` {
		t.Errorf("row 1002 attributes[reason] = %q, want %q", got, `he said "no"`)
	}
	if n := len(gotRows[2].attrs); n != 0 {
		t.Errorf("row 1003 attributes: want empty map, got %d entries (%v)", n, gotRows[2].attrs)
	}

	// The rows must actually be queryable through the table's own engine, and
	// exactly one row must carry a NULL expiry.
	var total, nullExpiry uint64
	if err := c.conn.QueryRow(ctx,
		"SELECT count(), countIf(expires_at IS NULL) FROM events WHERE project = ?", project,
	).Scan(&total, &nullExpiry); err != nil {
		t.Fatalf("aggregate query: %v", err)
	}
	if total != uint64(len(events)) || nullExpiry != 1 {
		t.Errorf("aggregates: count=%d (want %d), null_expiry=%d (want 1)", total, len(events), nullExpiry)
	}
}

// deleteByProject removes every row this test inserted, so a re-run starts
// clean. It prefers a synchronous lightweight DELETE (the rows are gone before
// the test returns) and falls back to an asynchronous ALTER ... DELETE
// mutation, waiting for that mutation to finish.
func deleteByProject(t *testing.T, c *Client, project string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	execErr := c.conn.Exec(ctx, "DELETE FROM events WHERE project = ?", project)
	if execErr != nil {
		t.Logf("lightweight DELETE cleanup failed (%v); falling back to ALTER ... DELETE", execErr)
		if err := c.conn.Exec(ctx, "ALTER TABLE events DELETE WHERE project = ?", project); err != nil {
			t.Errorf("cleanup of project %q failed: %v", project, err)
			return
		}
		waitForMutations(ctx, t, c)
	}

	// Prove the cleanup actually took effect rather than assuming it did.
	var remaining uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM events WHERE project = ?", project).Scan(&remaining); err != nil {
		t.Errorf("verify cleanup of %q: %v", project, err)
		return
	}
	if remaining != 0 {
		t.Errorf("cleanup of project %q left %d rows behind", project, remaining)
	}
}

// waitForMutations blocks until the events table has no unfinished mutations
// (ALTER ... DELETE is asynchronous), bounded by the caller's context.
func waitForMutations(ctx context.Context, t *testing.T, c *Client) {
	t.Helper()
	for {
		var pending uint64
		if err := c.conn.QueryRow(ctx,
			"SELECT count() FROM system.mutations WHERE database = currentDatabase() AND table = ? AND is_done = 0",
			EventsTable,
		).Scan(&pending); err == nil && pending == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Errorf("timed out waiting for events cleanup mutation: %v", ctx.Err())
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
