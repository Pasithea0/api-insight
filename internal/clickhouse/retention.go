package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// retentionMutationTimeout bounds a retention mutation request. ClickHouse
// mutations are asynchronous — the statement returns once the mutation is
// recorded, not once the rewrite has finished — so this bounds the request,
// not the work. The caller is a background goroutine, never an HTTP request.
const retentionMutationTimeout = 30 * time.Second

// projectRetentionStatements renders the two mutations that bring the mirror
// in line with a project's retention window.
//
// It is a pure function (no connection, no globals) so the statements can be
// asserted in tests without a running server, and so the day count can only
// reach the SQL as a validated integer literal: INTERVAL requires a literal,
// not a bound parameter, hence the same clamp-then-interpolate treatment
// eventsDDL gives its retention. ok is false when the window is unusable
// (non-positive or clamped away to nothing), so a caller can never emit
// `INTERVAL 0 DAY`.
func projectRetentionStatements(retentionDays int) (deleteStmt, updateStmt string, ok bool) {
	days := clampRetentionDays(retentionDays)
	if days <= 0 {
		return "", "", false
	}

	deleteStmt = fmt.Sprintf(
		"ALTER TABLE %s DELETE WHERE user_id = ? AND project = ? AND created_at < ?",
		EventsTable,
	)

	// Only rows that actually disagree with the window are rewritten, so the
	// mutation touches as few parts as possible. A NULL expires_at is
	// included: on this side of the mirror it means "never expires", and once
	// the project has a window those rows have to expire with everything else.
	updateStmt = fmt.Sprintf(`ALTER TABLE %s
UPDATE expires_at = created_at + INTERVAL %d DAY
WHERE user_id = ? AND project = ?
  AND (expires_at IS NULL OR expires_at != created_at + INTERVAL %d DAY)`,
		EventsTable, days, days)

	return deleteStmt, updateStmt, true
}

// SyncProjectRetention makes the mirror agree with a project's new retention
// window: rows older than the window are deleted, and surviving rows are given
// the matching expiry.
//
// Without this, a shortened window would only be enforced in Postgres: whenever
// ClickHouse is the raw-event read source (APP_CLICKHOUSE_READ_SOURCE, or
// ?store=clickhouse) the mirror would keep serving the older rows — exactly the
// data the operator just asked to delete. Because the mutations are
// asynchronous, a caller must treat success as "the change has been queued",
// not "the rows are gone"; the per-row TTL on expires_at cleans up the rest.
//
// Growing a window cannot resurrect rows the TTL has already removed here; the
// mirror is best-effort and has no backfill (see the package doc).
func (c *Client) SyncProjectRetention(ctx context.Context, userID, project string, retentionDays int) error {
	if c == nil || c.conn == nil {
		return errors.New("clickhouse: client is not connected")
	}
	if userID == "" || project == "" {
		return errors.New("clickhouse: userID and project are required")
	}
	deleteStmt, updateStmt, ok := projectRetentionStatements(retentionDays)
	if !ok {
		return fmt.Errorf("clickhouse: retention days must be positive, got %d", retentionDays)
	}

	ctx, cancel := context.WithTimeout(ctx, retentionMutationTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if err := c.conn.Exec(ctx, deleteStmt, userID, project, cutoff); err != nil {
		return fmt.Errorf("clickhouse: delete expired rows for %s/%s: %w", userID, project, err)
	}

	if err := c.conn.Exec(ctx, updateStmt, userID, project); err != nil {
		return fmt.Errorf("clickhouse: rewrite expiries for %s/%s: %w", userID, project, err)
	}
	return nil
}
