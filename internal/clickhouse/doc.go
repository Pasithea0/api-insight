// Package clickhouse is the analytical write path for api-insight.
//
// It owns exactly three things:
//
//   - Client: a thin wrapper around a clickhouse-go v2 connection with a
//     bounded timeout on every operation, so a hung ClickHouse can never
//     stall a caller (the ingest HTTP path in particular).
//   - Schema: an idempotent CREATE TABLE IF NOT EXISTS for the events table,
//     built by the pure function eventsDDL so it can be unit tested without
//     a server.
//   - Batch inserts: InsertEvents maps []dbpkg.Event onto the events table
//     using the clickhouse-go v2 batch API (PrepareBatch / Append / Send).
//
// Retention mirrors the PostgreSQL per-API-key retention model. Each row
// carries its own expires_at and expires individually:
//
//	TTL ifNull(expires_at, <far future>) DELETE WHERE ifNull(expires_at, ...) < now()
//
// A row with a NULL expires_at is substituted with a far-future sentinel so
// it never expires. When a positive retentionDays is configured, a second
// global age TTL is emitted as well:
//
//	TTL created_at + INTERVAL <n> DAY DELETE
//
// Two implementation notes that differ from a naive reading of the schema
// requirements, forced by ClickHouse (verified against 26.9.4):
//
//  1. A TTL expression may not be a Nullable column ("TTL expression result
//     column should have Date ... but has Nullable(DateTime64)").
//     `TTL expires_at DELETE` is therefore rejected while expires_at is
//     Nullable, so the per-row TTL wraps it in ifNull().
//  2. More than one bare DELETE TTL expression is rejected ("More than one
//     DELETE TTL expression without WHERE expression is not allowed"), so
//     the per-row expression carries a WHERE clause; this also preserves the
//     intended semantics (a NULL/far-future expiry is never deleted).
//
// This package deliberately does not import internal/config: Connect takes a
// plain DSN string and an int so call sites stay trivial.
package clickhouse
