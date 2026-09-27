package db

import (
	"context"
	"strings"
)

// RawEvents resolves raw-event reads to one of several backing stores.
//
// Postgres is the system of record and the default. ClickHouse is the
// optional long-history mirror, consulted only when it is wired in AND the
// operator has selected it as the read source. That caution is deliberate:
// the mirror has no backfill, so for the first days after it is enabled it
// holds LESS history than Postgres does, and silently switching every
// search over to it would lose results.
//
// A per-request override is honoured via ?store=clickhouse|postgres, so the
// choice can be validated from the dashboard without a redeploy. The
// parameter is named ?store= rather than ?source= because the export
// endpoint already uses ?source= to select WHAT to export.
type RawEvents struct {
	postgres   EventReader
	clickhouse EventReader // nil when the mirror is not in use
	def        string      // "postgres" or "clickhouse"
}

// NewRawEvents builds a resolver. ch may be nil, meaning ClickHouse is not
// available and every request is served by Postgres. defaultSource is
// honoured only when a ClickHouse reader is actually present.
func NewRawEvents(pg EventReader, ch EventReader, defaultSource string) *RawEvents {
	def := "postgres"
	if ch != nil && strings.EqualFold(strings.TrimSpace(defaultSource), "clickhouse") {
		def = "clickhouse"
	}
	return &RawEvents{postgres: pg, clickhouse: ch, def: def}
}

// ClickHouseAvailable reports whether a mirror is wired in.
func (r *RawEvents) ClickHouseAvailable() bool {
	return r != nil && r.clickhouse != nil
}

// DefaultSource names the store used when no override is requested.
func (r *RawEvents) DefaultSource() string {
	if r == nil {
		return "postgres"
	}
	return r.def
}

// Resolve picks the store for one request. override is the raw ?store=
// value; anything unrecognised falls back to the configured default.
func (r *RawEvents) Resolve(override string) EventReader {
	if r == nil {
		return nil
	}
	if r.clickhouse == nil {
		return r.postgres
	}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "clickhouse":
		return r.clickhouse
	case "postgres":
		return r.postgres
	}
	if r.def == "clickhouse" {
		return r.clickhouse
	}
	return r.postgres
}

// QueryEvents runs q against the resolved store and reports which store
// answered.
//
// If the chosen store errors, it retries against Postgres: a dashboard
// should degrade to the system of record rather than render an empty table
// because the mirror is unreachable. A query that legitimately returns no
// rows is NOT treated as a failure — only a transport/query error triggers
// the fallback.
func (r *RawEvents) QueryEvents(ctx context.Context, override string, q EventQuery) ([]Event, string, error) {
	primary := r.Resolve(override)
	events, err := primary.QueryEvents(ctx, q)
	if err == nil {
		return events, primary.SourceName(), nil
	}
	if r.postgres != nil && primary != r.postgres {
		if fallback, fbErr := r.postgres.QueryEvents(ctx, q); fbErr == nil {
			return fallback, r.postgres.SourceName(), nil
		}
	}
	return nil, primary.SourceName(), err
}

// EventByID reads a single event.
//
// A miss in one store is not conclusive: an event ingested before the mirror
// existed lives only in Postgres, and an event outside the raw retention
// window lives only in the mirror. So a miss falls through to the other
// store.
func (r *RawEvents) EventByID(ctx context.Context, override, id string) (*Event, error) {
	primary := r.Resolve(override)

	ev, err := primary.EventByID(ctx, id)
	if err != nil {
		if r.postgres != nil && primary != r.postgres {
			return r.postgres.EventByID(ctx, id)
		}
		return nil, err
	}
	if ev != nil || r.postgres == nil || primary == r.postgres {
		return ev, nil
	}
	return r.postgres.EventByID(ctx, id)
}
