package db

import (
	"context"
	"regexp"
	"time"
)

// SafeAttrKey validates an attribute key before it is interpolated into SQL.
// Attribute keys cannot be bound as parameters because they are identifiers
// (JSON paths / map subscripts), so every store must validate them itself
// rather than trusting the caller. This mirrors the identical guard in
// internal/http/handlers/metrics; each layer validates independently so a
// new caller cannot bypass it.
var SafeAttrKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// EventQuery is the shared filter for raw-event reads. It is the neutral
// shape both backing stores accept, so the HTTP layer does not have to know
// whether a request is served by Postgres or ClickHouse.
type EventQuery struct {
	// UserID scopes to one tenant. Empty means "all tenants", which is only
	// reachable by an admin (the HTTP layer decides that).
	UserID string

	// Project optionally narrows to one API key / project name.
	Project string

	// Status is "", "success" (<400) or "error" (>=400).
	Status string

	// AttrKey/AttrValue filter on one attribute pair when both are set.
	AttrKey   string
	AttrValue string

	// Field/Pattern describe a LIKE match. Field names a column ("route",
	// "method", "remote_ip", "status", "path") or an attribute key; Pattern
	// is an already-expanded LIKE pattern. Both must be set to take effect.
	Field   string
	Pattern string

	// Cutoff is the inclusive lower bound on created_at.
	Cutoff time.Time

	// Limit/Offset page the result set, newest first.
	Limit  int
	Offset int
}

// EventReader reads raw events from a backing store.
//
// Two implementations exist: PostgresReader (the system of record, complete
// history within the raw retention window) and the ClickHouse reader (the
// optional long-history mirror). Both must return identical rows for
// identical queries so the HTTP layer can switch between them safely.
type EventReader interface {
	// QueryEvents returns events matching q, newest first.
	QueryEvents(ctx context.Context, q EventQuery) ([]Event, error)

	// EventByID returns a single event, or (nil, nil) when it does not exist.
	EventByID(ctx context.Context, id string) (*Event, error)

	// SourceName identifies the store, for diagnostics and the ?source=
	// override.
	SourceName() string
}

// PostgresFieldExpr maps a dashboard "field" onto a Postgres expression.
// ok is false for a field name that must not be interpolated.
func PostgresFieldExpr(field string) (expr string, ok bool) {
	switch field {
	case "route", "remote_ip", "method":
		return field, true
	case "path":
		return "route", true
	case "status":
		return "CAST(status AS TEXT)", true
	default:
		if !SafeAttrKey.MatchString(field) {
			return "", false
		}
		return "attributes::jsonb ->> '" + field + "'", true
	}
}
