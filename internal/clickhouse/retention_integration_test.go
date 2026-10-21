package clickhouse

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	dbpkg "apiinsight/internal/db"
)

// TestIntegrationProjectRetention exercises SyncProjectRetention against a live
// ClickHouse: a shortened window must delete the mirror's older rows and move
// every surviving row onto the new expiry, while leaving other projects and
// other users alone.
//
// It needs a server, so it skips without CLICKHOUSE_TEST_DSN:
//
//	CLICKHOUSE_TEST_DSN='clickhouse://apiinsight:apiinsight@localhost:59000/apiinsight' \
//	    go test ./internal/clickhouse/ -run IntegrationProjectRetention -v -count=1
func TestIntegrationProjectRetention(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_TEST_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_TEST_DSN is not set; skipping live ClickHouse integration test")
	}

	ctx := context.Background()
	c, err := Connect(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Registered before the row cleanups below so that, with t.Cleanup's LIFO
	// ordering, the rows are deleted while the connection is still open.
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Unique names key this run's rows, so the test is re-runnable and its
	// cleanup can delete exactly what it inserted.
	suffix := time.Now().UnixNano()
	project := fmt.Sprintf("ch-retention-%d", suffix)
	peerProject := fmt.Sprintf("ch-retention-peer-%d", suffix)
	t.Cleanup(func() { deleteByProject(t, c, project) })
	t.Cleanup(func() { deleteByProject(t, c, peerProject) })

	const (
		owner      = "ch-retention-user"
		otherOwner = "ch-retention-other-user"
	)

	// Millisecond precision, because the assertions compare expires_at to
	// created_at + interval for equality and DateTime64(3) keeps milliseconds.
	now := time.Now().UTC().Truncate(time.Millisecond)
	at := func(d time.Duration) time.Time { return now.Add(d) }
	expiryIn := func(created time.Time, days int) *time.Time {
		e := created.Add(time.Duration(days) * 24 * time.Hour)
		return &e
	}

	old := at(-20 * 24 * time.Hour)
	recent := at(-2 * 24 * time.Hour)

	var events []dbpkg.Event
	add := func(user, proj, route string, created time.Time, expires *time.Time) {
		events = append(events, dbpkg.Event{
			ID:         uint(1000 + len(events)),
			CreatedAt:  created,
			ExpiresAt:  expires,
			UserID:     user,
			Project:    proj,
			Route:      route,
			Method:     "GET",
			Status:     200,
			DurationMs: 1,
		})
	}
	for i := 0; i < 3; i++ {
		// Written under a 30 day window, already outside a 7 day one.
		add(owner, project, "/older-than-window", old, expiryIn(old, 30))
		// Inside the window, still holding the longer promise.
		add(owner, project, "/long-promise", recent, expiryIn(recent, 30))
	}
	// "Never expires" on this side of the mirror; a project with a window has
	// to expire it like everything else.
	add(owner, project, "/no-expiry", recent, nil)
	// Control groups.
	add(otherOwner, project, "/other-user", old, expiryIn(old, 30))
	add(owner, peerProject, "/other-project", old, expiryIn(old, 30))

	if err := c.InsertEvents(ctx, events); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}

	if err := c.SyncProjectRetention(ctx, owner, project, 7); err != nil {
		t.Fatalf("SyncProjectRetention: %v", err)
	}
	// Mutations are asynchronous: wait for them to land before asserting.
	waitForMutations(ctx, t, c)

	count := func(sql string, args ...any) uint64 {
		t.Helper()
		var n uint64
		if err := c.conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return n
	}

	if got := count("SELECT count() FROM events WHERE user_id = ? AND project = ?", owner, project); got != 4 {
		t.Errorf("rows left in the edited project = %d, want 4", got)
	}
	if got := count("SELECT count() FROM events WHERE user_id = ? AND project = ? AND route = '/older-than-window'",
		owner, project); got != 0 {
		t.Errorf("rows older than the window remaining = %d, want 0", got)
	}
	// Covers the NULL expiry too: a row with no expiry can never satisfy an
	// equality comparison, so it is counted here if the rewrite skipped it.
	if got := count(`SELECT count() FROM events
		WHERE user_id = ? AND project = ?
		  AND (expires_at IS NULL OR expires_at != created_at + INTERVAL 7 DAY)`, owner, project); got != 0 {
		t.Errorf("%d surviving rows disagree with the 7 day window, want 0", got)
	}

	if got := count("SELECT count() FROM events WHERE user_id = ? AND project = ?", otherOwner, project); got != 1 {
		t.Errorf("another user's rows in the same project name = %d, want 1", got)
	}
	if got := count(`SELECT count() FROM events
		WHERE user_id = ? AND project = ? AND expires_at = created_at + INTERVAL 30 DAY`, otherOwner, project); got != 1 {
		t.Error("another user's expiry was rewritten by this project's edit")
	}
	if got := count("SELECT count() FROM events WHERE user_id = ? AND project = ?", owner, peerProject); got != 1 {
		t.Errorf("peer project rows = %d, want 1", got)
	}
	if got := count(`SELECT count() FROM events
		WHERE user_id = ? AND project = ? AND expires_at = created_at + INTERVAL 30 DAY`, owner, peerProject); got != 1 {
		t.Error("a peer project's expiry was rewritten")
	}

	// A 0 day window is the "no expiry" sentinel and must be refused before any
	// statement reaches the server.
	if err := c.SyncProjectRetention(ctx, owner, project, 0); err == nil {
		t.Error("a 0 day window was accepted")
	}
}
