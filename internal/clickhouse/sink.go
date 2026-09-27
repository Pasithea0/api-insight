package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	dbpkg "apiinsight/internal/db"
)

// insertColumns is the explicit column list used by the batch insert. Naming
// the columns keeps the mapping correct even if the DDL gains columns later.
const insertColumns = "id, created_at, expires_at, user_id, project, route, method, status, duration_ms, remote_ip, attributes"

// InsertEvents writes a batch of events to ClickHouse in a single native
// batch, using the clickhouse-go v2 batch API (PrepareBatch / Append / Send).
//
// An empty (or nil) slice is a no-op returning nil, so callers do not need to
// guard the call site. The whole operation is bounded by insertTimeout.
func (c *Client) InsertEvents(ctx context.Context, events []dbpkg.Event) error {
	if len(events) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, insertTimeout)
	defer cancel()

	batch, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+EventsTable+" ("+insertColumns+")")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare batch: %w", err)
	}

	for i := range events {
		if err := appendEvent(batch, events[i]); err != nil {
			return fmt.Errorf("clickhouse: append event %d: %w", i, err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("clickhouse: send batch of %d: %w", len(events), err)
	}
	return nil
}

// appendEvent maps one dbpkg.Event onto a batch row positionally, matching
// insertColumns.
func appendEvent(batch driver.Batch, ev dbpkg.Event) error {
	// expires_at is Nullable(DateTime64); a nil value is the "no expiry" case.
	var expiresAt any
	if ev.ExpiresAt != nil {
		expiresAt = ev.ExpiresAt.UTC()
	}

	return batch.Append(
		uint64(ev.ID),
		ev.CreatedAt.UTC(),
		expiresAt,
		ev.UserID,
		ev.Project,
		ev.Route,
		ev.Method,
		int32(ev.Status),
		ev.DurationMs,
		ev.RemoteIP,
		stringMap(ev.Attributes),
	)
}

// stringMap flattens arbitrary attribute values into the Map(String, String)
// column. Values may be numbers, booleans, nested maps or already-quoted
// strings, so each is rendered with fmt.Sprint; the map itself is never JSON
// encoded. A nil attributes map becomes an empty map, not a nil map.
func stringMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = ""
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}
