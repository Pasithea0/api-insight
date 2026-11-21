package db

import (
	"context"
	"sort"
	"strings"
)

// Store names understood by the raw-event resolver.
const (
	StorePostgres   = "postgres"
	StoreClickHouse = "clickhouse"
	StoreS3         = "s3"
)

// RawEvents resolves raw-event reads to one of several backing stores.
//
// Postgres is the system of record and the last resort. The other stores are
// consulted because they hold data Postgres does not:
//
//   - ClickHouse is an optional long-history mirror. It is chosen only when
//     it is wired in AND the operator has selected it, because the mirror has
//     no backfill and so holds LESS history than Postgres for the first days
//     after it is enabled.
//   - S3 is the object store the raw stream is written to when Postgres is
//     not the event store. There it holds everything and Postgres holds
//     nothing, which is the reverse of the mirror case.
//
// A per-request override is honoured via ?store=<name>, so the choice can be
// validated from the dashboard without a redeploy. The parameter is named
// ?store= rather than ?source= because the export endpoint already uses
// ?source= to select WHAT to export.
//
// Every accessor is nil-safe: a (*RawEvents)(nil) answers as a
// Postgres-only resolver rather than panicking, because the HTTP handlers are
// constructed once and must keep working if wiring is skipped.
type RawEvents struct {
	readers  map[string]EventReader
	scanners map[string]EventScanner

	// order is the fallback chain, default store first and Postgres last.
	order []string

	def string
}

// NewRawEvents builds a resolver.
//
// readers is every store that can serve a paged read; it must contain the
// system of record under StorePostgres. scanners holds the stores that
// cannot answer with SQL — S3 — and is consulted by the aggregate panels and
// the rollup worker.
//
// defaultSource is honoured when it names an available store, and otherwise
// falls back to Postgres.
func NewRawEvents(defaultSource string, readers map[string]EventReader, scanners map[string]EventScanner) *RawEvents {
	rs := make(map[string]EventReader, len(readers))
	for name, reader := range readers {
		if reader != nil {
			rs[name] = reader
		}
	}
	ss := make(map[string]EventScanner, len(scanners))
	for name, scanner := range scanners {
		if scanner != nil {
			ss[name] = scanner
		}
	}

	def := normalizeStoreName(defaultSource)
	if _, ok := rs[def]; !ok {
		def = StorePostgres
	}
	if _, ok := rs[def]; !ok {
		// Nothing conventional is wired in; use whatever there is so the
		// dashboard is not silently served by a nil reader.
		names := make([]string, 0, len(rs))
		for name := range rs {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			def = names[0]
		} else {
			def = StorePostgres
		}
	}

	// Fallback order: the default first, then every other store, with the
	// system of record last so a degraded read lands on the complete copy.
	var others []string
	for name := range rs {
		if name != def && name != StorePostgres {
			others = append(others, name)
		}
	}
	sort.Strings(others)

	order := make([]string, 0, len(rs))
	order = append(order, def)
	order = append(order, others...)
	if _, ok := rs[StorePostgres]; ok && def != StorePostgres {
		order = append(order, StorePostgres)
	}

	return &RawEvents{readers: rs, scanners: ss, order: order, def: def}
}

// normalizeStoreName lowercases and trims a store name so ?store= ClickHouse
// and ?store=clickhouse mean the same thing.
func normalizeStoreName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// Sources lists the available stores, default first.
func (r *RawEvents) Sources() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

// Available reports whether a named store is wired in.
func (r *RawEvents) Available(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.readers[normalizeStoreName(name)]
	return ok
}

// DefaultSource names the store used when no override is requested.
func (r *RawEvents) DefaultSource() string {
	if r == nil {
		return StorePostgres
	}
	return r.def
}

// Resolve picks the store for one request. override is the raw ?store= value;
// anything unrecognised falls back to the configured default.
func (r *RawEvents) Resolve(override string) EventReader {
	if r == nil {
		return nil
	}
	if reader, ok := r.readers[normalizeStoreName(override)]; ok {
		return reader
	}
	return r.readers[r.def]
}

// resolveName is Resolve's name-shaped twin, for diagnostics and for the
// scan-aware panels that need to know which store answered.
func (r *RawEvents) resolveName(override string) string {
	if r == nil {
		return StorePostgres
	}
	if _, ok := r.readers[normalizeStoreName(override)]; ok {
		return normalizeStoreName(override)
	}
	return r.def
}

// DefaultScanner returns the EventScanner for the default store and its
// name. A nil scanner means the default store answers with SQL, which is the
// signal the aggregate panels use to decide whether they can run a GROUP BY
// or have to stream and accumulate in Go.
func (r *RawEvents) DefaultScanner() (EventScanner, string) {
	if r == nil {
		return nil, StorePostgres
	}
	if sc, ok := r.scanners[r.def]; ok {
		return sc, r.def
	}
	return nil, r.def
}

// UsesScanner reports whether the default store must be read by streaming
// rather than by SQL.
func (r *RawEvents) UsesScanner() bool {
	sc, _ := r.DefaultScanner()
	return sc != nil
}

// QueryEvents runs q against the resolved store and reports which store
// answered.
//
// If the chosen store errors, the remaining stores are tried in fallback
// order and the first success wins: a dashboard should degrade to the system
// of record rather than render an empty table because one store is
// unreachable. A query that legitimately returns no rows is NOT treated as a
// failure — only a transport/query error triggers the fallback.
func (r *RawEvents) QueryEvents(ctx context.Context, override string, q EventQuery) ([]Event, string, error) {
	if r == nil {
		return nil, StorePostgres, nil
	}
	primary := r.Resolve(override)
	if primary == nil {
		return nil, StorePostgres, nil
	}

	events, err := primary.QueryEvents(ctx, q)
	if err == nil {
		return events, primary.SourceName(), nil
	}

	for _, name := range r.order {
		reader := r.readers[name]
		if reader == nil || reader == primary {
			continue
		}
		if fallback, fbErr := reader.QueryEvents(ctx, q); fbErr == nil {
			return fallback, reader.SourceName(), nil
		}
	}
	return nil, primary.SourceName(), err
}

// EventByID reads a single event.
//
// A miss in one store is not conclusive: an event ingested before a mirror
// existed lives only in Postgres, an event older than the raw retention
// window may live only in a mirror, and after a switch to the object store a
// pre-migration row lives only in Postgres. So a miss falls through to the
// other stores, newest concern first.
func (r *RawEvents) EventByID(ctx context.Context, override, id string) (*Event, error) {
	if r == nil {
		return nil, nil
	}
	primary := r.Resolve(override)
	if primary == nil {
		return nil, nil
	}

	ev, err := primary.EventByID(ctx, id)
	if err == nil && ev != nil {
		return ev, nil
	}

	// A miss (ev == nil && err == nil) and an error are both worth falling
	// through for: only one of the stores is authoritative for any given row.
	var lastErr error
	if err != nil {
		lastErr = err
	}
	for _, name := range r.order {
		reader := r.readers[name]
		if reader == nil || reader == primary {
			continue
		}
		other, otherErr := reader.EventByID(ctx, id)
		if otherErr != nil {
			lastErr = otherErr
			continue
		}
		if other != nil {
			return other, nil
		}
	}
	if lastErr != nil && err != nil {
		// Everything failed with a transport error; surface it rather than
		// pretending the event does not exist.
		return nil, lastErr
	}
	return nil, nil
}
