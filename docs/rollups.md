# Rollup growth, retention, and top-routes performance

The route-level rollup tables are the largest thing in this database and the
slowest thing the dashboard queries. Both facts have the same cause, and it is
a deliberate design choice rather than a defect.

## The design choice

`route` is the **full request path, query string included**:

```
/v4/media?tvdb_id=269653&season=8&episode=2&tvdb_episode_id=7928568&duration_ms=1293321
```

Every distinct query-parameter combination is its own route. That is what lets
you search a route by a real value — a title id, a season, an API key — with no
synthetic attribute to reconstruct, and it is what lets the dashboard show what
people are actually watching rather than "/media".

Stripping the query string from the stored route was implemented and **reverted
at the operator's direction**. Do not re-introduce it.

The cost is real and is inherited by everything that groups on route:
`route_buckets` accumulates roughly **10–15M rows per day**, and every
`GROUP BY route` pays it. The levers below are how it is bounded — not by
degrading what gets stored.

Metrics are the one place this *is* bounded, and separately: a Prometheus
counter holds one time series per distinct label combination for the life of
the process, so a label unique per request is unbounded RAM rather than a disk
cost. `MetricRouteLabel` reduces the route for the **label only**; it never
touches what is stored. Those counters are also not exposed — there is no
`promhttp` handler, `/metrics` is the dashboard page — so they are currently
pure overhead either way.

## What the tables hold

Measured on the production instance:

| Table | Rows | Size |
|---|---|---|
| `route_buckets` | 357,964,256 | 127 GB |
| `daily_route_buckets` | 171,073,184 | 58 GB |
| `metric_buckets` | 908 | 272 kB |
| `daily_metric_buckets` | 62 | 80 kB |

The route-level tables are the whole database; the error-rate and latency
history — the data with real long-range value — is a few hundred kilobytes.
The route tables grow at **~6 GB/day** (2.1 TB/year) and were pruned by nothing,
because `APP_RETENTION_DAYS` governs raw events and that is all it ever
governed.

## Bounding them

Two knobs, both defaulting to `0` = keep forever:

| Variable | Applies to |
|---|---|
| `APP_ROLLUP_RETENTION_DAYS` | `route_buckets`, `metric_buckets` (hourly) |
| `APP_DAILY_ROLLUP_RETENTION_DAYS` | `daily_route_buckets`, `daily_metric_buckets` (daily) |

Separate because the daily tables are what long ranges read, so *short hourly
window + daily history kept forever* is the useful pairing. The worker prunes
in batches of 50,000 rows keyed on `id` (never `ctid`, which is not unique
across a hypertable's chunks) and runs whatever the event store is.

Deleting does not return space to the operating system — it marks pages
reusable, so the relation stops growing, but the file stays allocated.
Reclaiming it needs a `TRUNCATE` or a `VACUUM FULL`.

## Reclaiming the disk

```sql
-- How bad is it
SELECT c.relname, pg_size_pretty(pg_total_relation_size(c.oid)) AS total
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind = 'r'
ORDER BY pg_total_relation_size(c.oid) DESC;

-- Reclaim immediately. This is route-level chart history, so it is a decision,
-- not a cleanup: metric_buckets and daily_metric_buckets are separate tables
-- and are untouched.
TRUNCATE route_buckets;
TRUNCATE daily_route_buckets;
ANALYZE route_buckets;
ANALYZE daily_route_buckets;
```

For a partial prune instead:

```sql
DELETE FROM route_buckets WHERE id IN (
  SELECT id FROM route_buckets
  WHERE bucket_start < now() - interval '90 days'
  LIMIT 50000
);
VACUUM FULL route_buckets;   -- ACCESS EXCLUSIVE lock for the duration
```

`VACUUM FULL` on the parent of a TimescaleDB hypertable reclaims nothing from
the chunks; on a plain table it is the right tool. Check which you have first.

## Top-routes performance

`GET /v1/metrics/top-routes` — the dashboard's **Top endpoints** — runs this on
its fast path:

```sql
SELECT route, SUM(total_count), SUM(error_count)
FROM route_buckets
WHERE bucket_start >= <cutoff> AND user_id = ?
GROUP BY route ORDER BY count DESC LIMIT 11;
```

Rows it must group, at ~12M rows/day:

| Range | Rows grouped |
|---|---|
| 1 day | ~12M |
| 7 days | ~85M |
| 30 days | ~358M |

**The range is the dominant factor.** The range selector has its `Max` option
marked `selected`, so the page opens on the widest range the view allows — the
top-endpoints panel groups the whole retained window on every load. That
setting is the first thing to change, and it is why the chart ceiling matters
so much here: raising it to a year turns a 7-day aggregation into a year-long
one.

The indexes on `route_buckets` are:

```
idx_route_bucket_unique              (user_id, project, route, bucket_start)
idx_route_buckets_bucket_start       (bucket_start)
idx_route_buckets_user_project_start (user_id, project, bucket_start)
```

The composite one matches the `WHERE`, but the aggregate needs `route`,
`total_count` and `error_count` from the heap, so every matching row costs a
heap fetch. At 85M rows against a 127 GB table that is the wall. Note also that
on the all-projects view there is no `project` equality, so the index's third
column cannot act as a range bound — the whole per-user range is scanned.

### What helps, in order

1. **Cache the response.** The same (user, project, range, status) is
   recomputed on every dashboard load and every poll. A short TTL turns a
   repeat load from tens of seconds into nothing.
2. **Do not open on the widest range.** Bound the default — 24h is enough to be
   useful — and let the selector widen deliberately.
3. **Set `APP_ROLLUP_RETENTION_DAYS`.** It bounds the prune work and stops the
   table growing without limit.
4. **Read `daily_route_buckets` beyond 48h.** ~5.7M rows/day against ~12M for
   the hourly table — only ~2x, because per-request routes rarely repeat across
   hours, so the daily rollup compresses far less than the usual 1/24.
5. **A covering index** — only after the tables are truncated. Including
   `route` makes the index roughly the size of the heap, which is not
   affordable on the disk that is already full.

### What does not help

- **An index on `route`.** The bottleneck is aggregating a huge number of
  distinct groups, not finding rows.
- **`LIMIT`.** The top 10 cannot be known without grouping everything first.
- **Approximation.** Not acceptable for a dashboard read as truth.
- **Normalising the stored route.** The fix that was already rejected.

### The structural option

If long-range top-endpoints must be genuinely fast *and* keep the full string,
the answer is a bounded pre-aggregate: a materialised leaderboard — the top N
routes per (user, project, day) — refreshed by a worker. Its key space is
bounded, so it stays small, and it is the only approach that makes a long-range
top-N cheap without changing what is stored. It is more moving parts than
caching, so it is worth doing only if caching and a sane default range prove
insufficient.
