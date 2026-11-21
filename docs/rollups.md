# Route normalisation and rollup retention

Two connected problems, both of which show up as "the database disk is full".

`APP_RETENTION_DAYS` governs **raw events**. It never governed the
pre-aggregated tables, and until this work nothing else did either. That is
how a deployment ended up with 185 GB of rollups behind an events table of
70 GB — and clearing the events freed less than a third of the disk.

## What the rollup tables actually held

Measured on a production instance before any of this changed:

| Table | Rows | Size |
|---|---|---|
| `route_buckets` | 357,964,256 | 127 GB |
| `events` | 77,607,048 | 70 GB |
| `daily_route_buckets` | 171,073,184 | 58 GB |
| `metric_buckets` | 861 | 272 kB |
| `daily_metric_buckets` | 62 | 80 kB |

The route-level tables are 100% of the database after the events are cleared,
and they grow at **~6 GB/day (2.1 TB/year)**. The metric-level tables — the
error-rate and latency history, the data with real long-range value — are a few
hundred kilobytes.

## Why

`route_buckets` is keyed on `(user_id, project, route, bucket_start)`. It is a
rollup: it exists to be *smaller* than the raw events. It was larger, because
`route` was the full request URL rather than an endpoint:

```
/v4/media?tvdb_id=269653&season=8&episode=2&tvdb_episode_id=7928568&duration_ms=1293321
/v3/media?tmdb_id=71789&season=1&episode=1&duration_ms=2609648
```

In one hour bucket: **160,890 rows, 160,890 distinct routes**, 78% of them
with a count of 1. Ignore the query string and those rows collapse to **21
distinct paths** — a factor of 7,661. `/v3/media` alone was 81,270 URL
variants carrying 139,979 requests.

A per-request value in the grouping key means:

- one rollup row per request, plus a 52 GB unique index enforcing uniqueness on
  values that were never equal in the first place;
- **Top Routes ranks URL noise.** The signal is there — 139,979 requests to
  `/v3/media` in an hour — but the feature surfaces per-episode URLs.

The same value was also used as a Prometheus label, which is the same mistake
in the process's own memory: one time series per distinct label combination,
growing without bound.

## The fix

Two fields, two jobs:

- **`route`** is the endpoint, query string stripped. Everything that groups —
  the rollup keys, the Prometheus labels — uses this. Two requests with
  different parameters are one endpoint.
- **`path`** is the request exactly as the client reported it, query string
  included. It is deliberately not indexed and not aggregated: its cardinality
  is per-request, which is precisely what must not reach a rollup key. It is
  kept because it is the source data `route` is derived from, so a change to
  the normalisation rule can be replayed over history.

Normalisation (`internal/db/route.go`) strips the query string and fragment,
drops scheme and host from a full URL, and ignores trailing slashes. It
deliberately does **not** collapse path segments: turning
`/v1/metrics/traffic` into `/v1/metrics/*` would merge genuinely different
endpoints, and unlike a query string there is no way to tell which segments are
identifiers. A client that knows its own templates should send `route`
explicitly, and it is used as given:

```json
{"path": "/v4/shows/12345/episodes/678?lang=en",
 "route": "/v4/shows/:id/episodes/:episode"}
```

Effect: the rollup tables go from ~6 GB/day to **~0.8 MB/day** — about
291 MB/year — so "keep everything forever" becomes affordable, which is what
`APP_RETENTION_DAYS=0` was supposed to mean.

## Bounding them anyway

Normalisation makes the tables small; it does not make them bounded. Two knobs
do, and both default to `0` (keep forever):

| Variable | Applies to |
|---|---|
| `APP_ROLLUP_RETENTION_DAYS` | `route_buckets`, `metric_buckets` (hourly) |
| `APP_DAILY_ROLLUP_RETENTION_DAYS` | `daily_route_buckets`, `daily_metric_buckets` (daily) |

They are separate because the dashboard falls back to the daily tables for
ranges beyond 48h: those are the long-range record, so a short hourly window
with daily history kept indefinitely is a sensible pairing.

The worker prunes in batches of 50,000 rows, keyed on `id` (never `ctid`,
which is not unique across a hypertable's chunks), and runs whatever the event
store is — the rollups are always in Postgres.

## Reclaiming the disk the old rows occupy

Deleting does not return space to the operating system. It marks pages
reusable, so the relation stops growing, but the file stays allocated.
Reclaiming it needs one of these:

### Option A — truncate the route tables (recommended here)

The existing rows are keyed on un-normalised routes, so they cannot be joined
to anything the new code produces and their historical value is close to zero.
`metric_buckets` and `daily_metric_buckets` — the error-rate and latency
history, and the only part with long-range value — are separate tables and are
untouched.

```sql
-- Frees the space immediately. Rebuilds going forward with real endpoints.
TRUNCATE route_buckets;
TRUNCATE daily_route_buckets;
ANALYZE route_buckets;
ANALYZE daily_route_buckets;
```

Cost: route-level chart history. Long-range error-rate and latency charts are
unaffected. Route-level history can be rebuilt from stored events for as far
back as they exist — widen `APP_S3_STARTUP_SCAN_HOURS` before restarting, or
let the hourly worker repopulate as it runs.

### Option B — prune by age and rewrite

Keeps the history you choose, at the cost of a locked rewrite.

```sql
-- 1. How bad is it
SELECT c.relname, pg_size_pretty(pg_total_relation_size(c.oid)) AS total
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind = 'r'
ORDER BY pg_total_relation_size(c.oid) DESC;

-- 2. Prune, in batches, until it reports 0 rows: repeat with a smaller LIMIT
--    if you want to bound each transaction further.
DELETE FROM route_buckets WHERE id IN (
  SELECT id FROM route_buckets
  WHERE bucket_start < now() - interval '90 days'
  LIMIT 50000
);

-- 3. Reclaim. ACCESS EXCLUSIVE lock for the duration — the table is
--    unavailable to the dashboard while this runs.
VACUUM FULL route_buckets;
```

`VACUUM FULL` on the parent table of a TimescaleDB hypertable reclaims nothing
from the chunks; on a plain table it is the right tool. Check which you have
before choosing.

## Checking the fix took effect

```sql
-- Should be a small number of endpoints, not one per request.
SELECT bucket_start, count(*) AS rows, count(DISTINCT route) AS routes
FROM route_buckets
WHERE bucket_start >= now() - interval '2 hours'
GROUP BY 1 ORDER BY 1 DESC;

-- A rollup that is working has counts well above 1.
SELECT max(total_count), round(avg(total_count)::numeric, 2)
FROM route_buckets
WHERE bucket_start >= now() - interval '2 hours';
```

Before: 160,890 rows / 160,890 routes, average count 1.71.
Expected after: a handful of routes per hour with average counts in the
thousands.
