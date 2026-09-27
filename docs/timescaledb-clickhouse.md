# Storage migration: TimescaleDB + ClickHouse

**Status:** implemented and verified locally against TimescaleDB 2.30.1 / PG 17
and ClickHouse 26.9.4. **Not yet deployed** — production needs a maintenance
window (see [Production runbook](#production-runbook)).

## Why

`events` was a single unbounded Postgres table at ~13M rows/day. Every
dashboard query degraded as it grew, and retention was a mass `DELETE` that
bloated the table and needed `VACUUM`. Three separate problems:

1. queries scan data they don't need,
2. old data costs full price to store,
3. expiring old data is expensive and leaves dead tuples behind.

## Architecture

| Concern | Lives in | Notes |
|---|---|---|
| Users, API keys, ownership | Postgres | unchanged |
| Raw events, recent window | Postgres **hypertable** | time-partitioned, columnstore after `APP_TIMESCALE_COLUMNSTORE_AFTER` |
| Hourly/daily rollups | Postgres **continuous aggregates** → bucket tables | dashboard keeps reading the bucket tables unchanged |
| Long-range raw queries / search | **ClickHouse** (optional, off by default) | columnar mirror |

ClickHouse **mirrors** Postgres rather than replacing it. Postgres stays the
system of record, so turning ClickHouse off (or it being unreachable) cannot
lose data or break a request — the mirror is best-effort and drops batches
rather than blocking ingest if it falls behind. The trade-off is that raw
events are stored twice when enabled: Postgres keeps the recent window and the
rollups, ClickHouse keeps the cheap long tail.

## What changed in code

- `internal/db/timescale.go` *(new)* — extension detection, primary-key
  rewrite, `create_hypertable`, columnstore policy, chunk-drop retention,
  continuous aggregates.
- `internal/db/retention.go` — **bug fix**, see below.
- `internal/db/aggregation.go` — rollup worker reads the continuous
  aggregates when available; `jsonb_typeof` guard fix.
- `internal/db/migrations.go` — no longer issues `CREATE INDEX CONCURRENTLY`
  on a hypertable (TimescaleDB rejects it).
- `internal/db/db.go` — TimescaleDB setup wired into `Connect`; `AutoMigrate`
  is now hypertable-aware; `DailyRouteBucket` added to `AutoMigrate`.
- `internal/db/models.go` — `events.created_at` is explicitly `NOT NULL`.
- `internal/clickhouse/` *(new)* — client, DDL, batch sink.
- `internal/http/handlers/batch_writer.go` — optional `EventSink` mirroring.
- `main.go` — ClickHouse wiring with credential redaction.
- `docker-compose.dev.yml` *(new)* — local TimescaleDB + ClickHouse stack.

## Bugs found and fixed while doing this

These were all live in production, not introduced by this work.

1. **Retention deleted live rows (data loss).** `retention.go` used
   `DELETE ... WHERE ctid IN (SELECT ctid FROM events ...)`. `ctid` is a
   physical row address that is unique only *within a single table*, and on a
   hypertable every chunk is a separate table. The same `ctid` therefore
   matches rows in every chunk. Reproduced: 10 expired rows in one chunk plus
   10 live rows in another → the old statement deleted **10 rows, 5 of them
   live**. Retention is now keyed on `id`, which is unique across the whole
   hypertable. Covered by `TestRetentionDoesNotDeleteLiveRows`.

2. **Second boot would have failed to start.** `events.created_at` becomes
   non-nullable on a hypertable, and GORM's `AutoMigrate` (which runs on
   every startup) tried to reconcile that with
   `ALTER COLUMN created_at DROP NOT NULL`, which TimescaleDB rejects with
   `SQLSTATE TS101`. So the deploy *after* the migration would crash-loop on
   startup. Fixed in the model (`not null`) and, defensively, by making
   `Connect` tolerate TimescaleDB partition restrictions instead of exiting.

3. **Attribute aggregation aborted the whole pass.** A JSON `null` scalar is
   not SQL NULL, so it passed the `attributes IS NOT NULL AND attributes !=
   '{}'` guard and then `jsonb_object_keys()` raised *"cannot call
   jsonb_object_keys on a scalar"*, failing every bucket in that run. Now
   guarded on `jsonb_typeof(attributes) = 'object'`.

4. **`daily_route_buckets` was never created.** `DailyRouteBucket` was
   missing from the `AutoMigrate` list, so the table did not exist and the
   daily route rollup and its long-range dashboard reads could not work.

## Production runbook

This is **not** a hot-swap. The hypertable conversion takes an
`ACCESS EXCLUSIVE` lock, rebuilds the primary-key index and moves existing
rows through chunks. Plan a window.

### 1. Back up first

Take a Postgres backup you have actually restored before. The PK rewrite is
the one step that is not trivially reversible.

### 2. Swap the Postgres image (Coolify)

Replace the Postgres service image with one that bundles TimescaleDB:

```
timescale/timescaledb:latest-pg17
```

The extension must be preloaded by the server process. The official image
already sets `shared_preload_libraries=timescaledb`; if you run a custom
`postgresql.conf`, add it yourself. Keep the same data volume so it is an
in-place upgrade, not a new database.

### 3. Set environment

From `.env.example`: `APP_TIMESCALE_ENABLED=true` (default),
`APP_TIMESCALE_CHUNK_INTERVAL=24h`, `APP_TIMESCALE_COLUMNSTORE_AFTER=14`,
`APP_RAW_RETENTION_DAYS=30`, `APP_TIMESCALE_CONTINUOUS_AGGREGATES=true`.

`APP_RAW_RETENTION_DAYS` is the key knob for capping table size: it drops
whole chunks, so it is cheap. Set it at or below your longest legitimate
per-key retention so you never silently cut a customer's data short.

Leave `APP_CLICKHOUSE_ENABLED=false` for this step — do one change at a time.

### 4. Deploy, then watch the conversion

The app applies the migration at startup. Expect a pause on first boot while
the PK is rewritten and existing rows move into chunks; `migrate_data =>
true` is doing real work. Watch for:

```
timescale: rewriting events primary key to (id, created_at) — this rebuilds the PK index under an exclusive lock
timescale: converted events to a hypertable (chunk interval 24h0m0s)
timescale: ready (v..., hypertable=true, chunks=..., columnstore=..., caggs=true)
```

### 5. Verify

```sql
-- is it a hypertable?
SELECT hypertable_name, num_chunks, compression_enabled
FROM timescaledb_information.hypertables WHERE hypertable_name = 'events';

-- is the primary key hypertable-compatible?
SELECT a.attname FROM pg_index i
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
WHERE i.indrelid = 'events'::regclass AND i.indisprimary;

-- are the rollups materialising?
SELECT count(*) FROM events_hourly;
SELECT * FROM timescaledb_information.jobs WHERE proc_name LIKE '%policy%';
```

Then confirm the dashboard renders, and that a rollup pass produces sane
numbers for a completed hour.

### 6. Add ClickHouse (separate change)

Deploy the ClickHouse service, set `APP_CLICKHOUSE_DSN` and
`APP_CLICKHOUSE_ENABLED=true`. Startup logs the redacted DSN and
`clickhouse mirror enabled`. Rows start flowing immediately; there is **no
backfill** of history, so ClickHouse only holds events ingested after it was
switched on.

## Known limitations

- **Columnstore must outlive per-key retention.** Deleting individual rows
  from a columnstore chunk works (TimescaleDB decompresses as needed) but is
  expensive. Keep `APP_TIMESCALE_COLUMNSTORE_AFTER` above your longest
  per-key retention window.
- **ClickHouse TTL needs `ifNull`.** `TTL expires_at DELETE` is rejected
  (`Code 450 BAD_TTL_EXPRESSION`) because a TTL expression cannot be
  `Nullable`. The DDL wraps it: `TTL ifNull(expires_at, <far future>) DELETE
  WHERE ifNull(expires_at, <far future>) < now()`. Verified working.
- **ClickHouse has no backfill** (see step 6).
- **The ClickHouse read path is not wired.** Events mirror into ClickHouse
  but no dashboard or `/v1/metrics/*` endpoint reads from it yet, so today
  ClickHouse is write-only storage that you query by hand. Wiring
  search/export to it is the next batch of work.
- **No index adds on a live hypertable.** `CREATE INDEX CONCURRENTLY` is not
  permitted once partitioned, so new indexes on `events` are now
  maintenance-window operations rather than startup side effects.

## Testing

Hermetic by default; integration tests skip unless pointed at a database.

```bash
# unit tests only (no database needed)
go test ./...

# TimescaleDB integration, against a throwaway database
APIINSIGHT_TEST_DATABASE_URL='postgres://apiinsight:apiinsight@localhost:55434/apiinsight_test?sslmode=disable' \
  go test ./internal/db/ -run Timescale -v -count=1

# ClickHouse integration
CLICKHOUSE_TEST_DSN='clickhouse://apiinsight:apiinsight@localhost:59000/apiinsight' \
  go test ./internal/clickhouse/ -v -count=1
```

Local stack: `docker compose -f docker-compose.dev.yml up -d`
(tear down and wipe with `... down -v`).

**Never point `APIINSIGHT_TEST_DATABASE_URL` at production** — the tests
convert `events` to a hypertable and run the retention worker.
