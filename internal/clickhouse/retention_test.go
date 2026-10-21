package clickhouse

import (
	"context"
	"strings"
	"testing"
)

// TestProjectRetentionStatements asserts the two mutations that bring the
// mirror in line with a retention edit. It needs no server: the statements are
// rendered by a pure function precisely so this can be pinned in a test.
//
// The binding count matters as much as the text. ClickHouse binds `?` by
// position, so a statement that promises three placeholders while the call
// passes two values fails at runtime, in a background goroutine, on the one
// code path nobody exercises by hand.
func TestProjectRetentionStatements(t *testing.T) {
	t.Run("delete and rewrite target one project only", func(t *testing.T) {
		del, upd, ok := projectRetentionStatements(7)
		if !ok {
			t.Fatal("a 7 day window must be renderable")
		}

		for _, want := range []string{
			"ALTER TABLE " + EventsTable + " DELETE",
			"user_id = ?",
			"project = ?",
			"created_at < ?",
		} {
			if !strings.Contains(del, want) {
				t.Errorf("delete statement missing %q:\n%s", want, del)
			}
		}
		if got := strings.Count(del, "?"); got != 3 {
			t.Errorf("delete statement has %d placeholders, want 3 (userID, project, cutoff):\n%s", got, del)
		}

		for _, want := range []string{
			"ALTER TABLE " + EventsTable,
			"UPDATE expires_at = created_at + INTERVAL 7 DAY",
			"WHERE user_id = ? AND project = ?",
			// Only rows that disagree with the window are touched, and a NULL
			// expiry counts as disagreeing: on the mirror side it means "never
			// expires", and a project with a window must expire its rows.
			"expires_at IS NULL OR expires_at != created_at + INTERVAL 7 DAY",
		} {
			if !strings.Contains(upd, want) {
				t.Errorf("update statement missing %q:\n%s", want, upd)
			}
		}
		if got := strings.Count(upd, "?"); got != 2 {
			t.Errorf("update statement has %d placeholders, want 2 (userID, project):\n%s", got, upd)
		}
	})

	t.Run("an unusable window renders nothing", func(t *testing.T) {
		// 0 is the API key model's "use the configured default" sentinel and
		// never a window of its own; emitting `INTERVAL 0 DAY` would delete
		// everything and expire it on arrival.
		for _, days := range []int{0, -1, -30} {
			del, upd, ok := projectRetentionStatements(days)
			if ok {
				t.Errorf("days=%d produced a statement: %q / %q", days, del, upd)
			}
		}
	})

	t.Run("absurd windows are clamped, never interpolated unchecked", func(t *testing.T) {
		_, upd, ok := projectRetentionStatements(999999)
		if !ok {
			t.Fatal("999999 must clamp to the cap, not fail")
		}
		if !strings.Contains(upd, "INTERVAL 3650 DAY") {
			t.Errorf("retention above the cap was not clamped to 3650:\n%s", upd)
		}
		if strings.Contains(upd, "999999") {
			t.Errorf("unclamped retention leaked into the statement:\n%s", upd)
		}
	})
}

// TestSyncProjectRetentionGuards proves the argument checks fire before the
// connection is touched: a zero-value Client has a nil connection and would
// panic if a guard were missing.
func TestSyncProjectRetentionGuards(t *testing.T) {
	c := &Client{}
	if err := c.SyncProjectRetention(context.Background(), "user-1", "project-1", 7); err == nil {
		t.Error("a client with no connection must report an error, not panic")
	}

	var nilClient *Client
	if err := nilClient.SyncProjectRetention(context.Background(), "user-1", "project-1", 7); err == nil {
		t.Error("a nil client must report an error, not panic")
	}
}
