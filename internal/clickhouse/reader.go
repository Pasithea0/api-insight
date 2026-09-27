package clickhouse

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/datatypes"

	dbpkg "apiinsight/internal/db"
)

// queryTimeout bounds every read so a slow ClickHouse cannot pin a
// dashboard request open indefinitely.
const queryTimeout = 30 * time.Second

// maxQueryLimit caps a single read. The HTTP layer already clamps its own
// limits; this is defence in depth so a direct caller cannot ask for an
// unbounded scan.
const maxQueryLimit = 5000

// selectColumns is the explicit projection shared by every read. Naming the
// columns keeps the positional scan below correct even if the DDL grows.
const selectColumns = "id, created_at, expires_at, user_id, project, route, method, status, duration_ms, remote_ip, attributes"

// safeAttrKey mirrors the identifier guard in internal/db. Attribute keys
// are interpolated (they are map subscripts, not bindable parameters), so
// this package validates them itself rather than trusting the caller.
var safeAttrKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// SourceName implements db.EventReader.
func (c *Client) SourceName() string { return "clickhouse" }

// chFieldExpr maps a dashboard "field" onto a ClickHouse expression.
//
// ClickHouse stores attributes as Map(String, String), so an attribute
// lookup is a subscript rather than a JSON path, and every attribute value
// is a string. ok is false for a field name that must not be interpolated.
func chFieldExpr(field string) (expr string, ok bool) {
	switch field {
	case "route", "remote_ip", "method":
		return field, true
	case "path":
		// Routes are stored with their query string, so path and route are
		// the same column here, exactly as in Postgres.
		return "route", true
	case "status":
		return "toString(status)", true
	default:
		if !safeAttrKey.MatchString(field) {
			return "", false
		}
		return "attributes['" + field + "']", true
	}
}

// buildQuery renders the SELECT for an EventQuery.
//
// It is a pure function so the generated SQL and its bind arguments can be
// asserted without a server. Attribute keys and search field names are
// IDENTIFIERS, not bindable parameters, so they are validated against
// safeAttrKey and rejected rather than interpolated unchecked — that is the
// injection boundary for this package. Everything user-supplied that can be
// bound (values, patterns, ids) travels as an argument.
//
// It returns the statement, its arguments, and the effective row limit.
func buildQuery(q dbpkg.EventQuery) (sql string, args []any, limit int, err error) {
	var where []string
	where = append(where, "created_at >= ?")
	args = append(args, q.Cutoff)

	if q.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, q.UserID)
	}
	if q.Project != "" {
		where = append(where, "project = ?")
		args = append(args, q.Project)
	}
	switch q.Status {
	case "success":
		where = append(where, "status < 400")
	case "error":
		where = append(where, "status >= 400")
	}
	if q.AttrKey != "" && q.AttrValue != "" {
		if !safeAttrKey.MatchString(q.AttrKey) {
			return "", nil, 0, fmt.Errorf("clickhouse: invalid attribute key %q", q.AttrKey)
		}
		where = append(where, "attributes['"+q.AttrKey+"'] = ?")
		args = append(args, q.AttrValue)
	}
	if q.Field != "" && q.Pattern != "" {
		expr, ok := chFieldExpr(q.Field)
		if !ok {
			return "", nil, 0, fmt.Errorf("clickhouse: invalid search field %q", q.Field)
		}
		where = append(where, expr+" LIKE ?")
		args = append(args, q.Pattern)
	}

	limit = q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	sql = fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY created_at DESC LIMIT ? OFFSET ?",
		selectColumns, EventsTable, strings.Join(where, " AND "),
	)
	args = append(args, limit, offset)
	return sql, args, limit, nil
}

// QueryEvents implements db.EventReader against the ClickHouse mirror.
//
// The generated SQL is deliberately close to the Postgres equivalent so the
// two stores agree row for row. Two differences are inherent to the storage
// model and are documented in docs/timescaledb-clickhouse.md: attribute
// values are compared as strings (ClickHouse has no JSON typing), and LIKE
// is byte-wise like Postgres rather than locale aware.
func (c *Client) QueryEvents(ctx context.Context, q dbpkg.EventQuery) ([]dbpkg.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	sql, args, limit, err := buildQuery(q)
	if err != nil {
		return nil, err
	}

	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query events: %w", err)
	}
	defer rows.Close()

	events := make([]dbpkg.Event, 0, limit)
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: iterate events: %w", err)
	}
	return events, nil
}

// EventByID implements db.EventReader. It returns (nil, nil) when the row
// does not exist, matching the Postgres reader.
func (c *Client) EventByID(ctx context.Context, id string) (*dbpkg.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	sql := fmt.Sprintf("SELECT %s FROM %s WHERE id = ? LIMIT 1", selectColumns, EventsTable)
	rows, err := c.conn.Query(ctx, sql, id)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query event %s: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("clickhouse: query event %s: %w", id, err)
		}
		return nil, nil
	}
	ev, err := scanEvent(rows)
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// rowScanner is the subset of driver.Rows used by scanEvent, so the scan
// logic can be exercised without a live connection.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEvent reads one row positionally, matching selectColumns.
//
// Positional scanning is used rather than struct scanning because the
// column names are snake_case and the Go fields are not; an explicit scan
// removes any dependence on a name-matching convention.
func scanEvent(rows rowScanner) (dbpkg.Event, error) {
	var (
		id         uint64
		createdAt  time.Time
		expiresAt  *time.Time
		userID     string
		project    string
		route      string
		method     string
		status     int32
		durationMs int64
		remoteIP   string
		attrs      map[string]string
	)

	if err := rows.Scan(
		&id, &createdAt, &expiresAt, &userID, &project, &route,
		&method, &status, &durationMs, &remoteIP, &attrs,
	); err != nil {
		return dbpkg.Event{}, fmt.Errorf("clickhouse: scan event: %w", err)
	}

	// Attribute values are stored as strings; hand them back in the same
	// JSONMap shape the Postgres path produces so the HTTP layer needs no
	// branch. A nil map becomes an empty one, matching ingest behaviour.
	jsonAttrs := datatypes.JSONMap{}
	for k, v := range attrs {
		jsonAttrs[k] = v
	}

	return dbpkg.Event{
		ID:         uint(id),
		CreatedAt:  createdAt.UTC(),
		ExpiresAt:  expiresAt,
		UserID:     userID,
		Project:    project,
		Route:      route,
		Method:     method,
		Status:     int(status),
		DurationMs: durationMs,
		RemoteIP:   remoteIP,
		Attributes: jsonAttrs,
	}, nil
}
