package clickhouse

import "fmt"

// EventsTable is the ClickHouse table that backs the analytical event store.
const EventsTable = "events"

// maxRetentionDays caps the global age TTL. The value is inlined into DDL as
// an integer literal (a DDL identifier, not a bind parameter), so it must be
// validated rather than interpolated unchecked.
const maxRetentionDays = 3650

// farFutureLiteral is the substitution used for rows whose expires_at is NULL.
// ClickHouse requires a non-Nullable Date/DateTime expression for a TTL, so a
// NULL per-row expiry is mapped to a sentinel far enough out that it never
// fires in practice.
const farFutureLiteral = "toDateTime64('2299-12-31 23:59:59', 3, 'UTC')"

// clampRetentionDays normalises a configured retention into the accepted
// range. Non-positive values disable the global age TTL; absurd values are
// clamped to maxRetentionDays so nothing unvalidated reaches the DDL.
func clampRetentionDays(days int) int {
	switch {
	case days <= 0:
		return 0
	case days > maxRetentionDays:
		return maxRetentionDays
	default:
		return days
	}
}

// eventsDDL renders the CREATE TABLE statement for the events table.
//
// It is a pure function (no connection, no globals) so the DDL can be
// asserted in tests without a running server. The statement is idempotent:
// it is emitted as CREATE TABLE IF NOT EXISTS.
//
// retentionDays > 0 adds a global age TTL on top of the per-row TTL, so a row
// is removed at the earlier of "its own expiry" and "retentionDays old".
func eventsDDL(retentionDays int) string {
	days := clampRetentionDays(retentionDays)

	// Per-row retention: each row expires on its own expires_at; a NULL
	// expiry is treated as far-future so those rows never expire.
	ttl := fmt.Sprintf(
		"TTL ifNull(expires_at, %s) DELETE WHERE ifNull(expires_at, %s) < now()",
		farFutureLiteral, farFutureLiteral,
	)

	// Global age cap, emitted only when configured. days is clamped above,
	// so the interpolated value is always a small, validated integer.
	if days > 0 {
		ttl += fmt.Sprintf(", created_at + INTERVAL %d DAY DELETE", days)
	}

	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s
(
    id          UInt64,
    created_at  DateTime64(3, 'UTC'),
    expires_at  Nullable(DateTime64(3, 'UTC')),
    user_id     String CODEC(ZSTD(3)),
    project     String CODEC(ZSTD(3)),
    route       String CODEC(ZSTD(3)),
    method      String CODEC(ZSTD(3)),
    status      Int32,
    duration_ms Int64 CODEC(ZSTD(3)),
    remote_ip   String CODEC(ZSTD(3)),
    attributes  Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (user_id, project, created_at, id)
%s`, EventsTable, ttl)
}
