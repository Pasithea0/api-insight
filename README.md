# API Insight – Lightweight API Analytics Sidecar

API Insight is a **lightweight companion service** for collecting and visualizing real-time API traffic analytics from your other services.  
Your existing APIs send request events to this service via a small middleware + API key, and API Insight handles **ingestion, storage, aggregation, and dashboards**.

![API Insight Preview](preview.png)

## Philosophy

This projects aims to be a simple, fast, and lightweight analytics dashboard for viewing your API's traffic. Great for identifying broken, popular, and critical endpoints.

---

## Quick Start

1. **Clone and build**:
   ```bash
   git clone https://github.com/Pasithea0/api-insight
   cd api-insight
   go mod download
   go build -o apiinsight .
   ```

2. **Configure environment** (copy `.env.example` to `.env` and set a PostgreSQL URL):
   ```bash
   APP_ADMIN_USER=admin
   APP_ADMIN_PASSWORD=changeme
   APP_DATABASE_URL=postgres://user:password@localhost:5432/postgres?sslmode=disable
   APP_RETENTION_DAYS=30
   APP_LISTEN_ADDR=:8080
   ```

3. **Run**:
   ```bash
   ./apiinsight
   ```

4. **Access**:
   - Dashboard: http://localhost:8080/
   - Login with credentials from `.env`
   - Health check: http://localhost:8080/healthz

--- 

## Usage

Sending events
POST request batches to /v1/events with a Bearer API key.
Use an API key from Settings. Send a POST to your API Insight base URL with path /v1/events, header Authorization: Bearer PROJECT_API_KEY, and Content-Type: application/json.

Request body:
```
{
  "events": [
    {
      "path": "/api/users",                     // required
      "duration_ms": 45,                        // required
      "method": "GET",                          // optional
      "status": 200,                            // optional
      "timestamp": "2024-01-28T12:00:00Z",      // optional
      "remote_ip": "192.0.2.1",                 // optional
      "attributes": {                           // anything can go in attributes!
        "env": "production",
        "region": "us-east-1"
      }
    }
  ]
}
```
The fields shown are required: path, duration_ms, and optionally method, status, timestamp, remote_ip. Any additional data can be added in the attributes block as key/value JSON (e.g. env, region, IDs).

Public API access
Each project can also expose a separate read-only public API key from Settings. This key is distinct from the write key used for `/v1/events`, and can be rotated independently.

All endpoints under `/v1/public/*` are secured with that public API key.

Currently available public endpoint:

```bash
curl "http://localhost:8080/v1/public/top-endpoints?public_key=PROJECT_PUBLIC_KEY&range=7d&status=error&count=10"
```

Supported query parameters:

- `public_key` or `key`: the project's public API key
- `range`: compact time range such as `24h` or `7d`
- `status`: `all`, `success`, or `error`
- `count`: number of routes to return, capped at `100`

Example response:

```json
{
  "project": "payments-api",
  "range": "7d",
  "status": "error",
  "count": 10,
  "routes": [
    {
      "route": "/api/orders/submit",
      "count": 41,
      "statuses": [
        { "status": 500, "count": 28 },
        { "status": 429, "count": 13 }
      ]
    }
  ],
  "total": 14,
  "has_more": true,
  "public_api": true
}
```

Public endpoints are intentionally limited to safe aggregated data. The current `top-endpoints` endpoint does not expose individual events, IP addresses, referers, or user agents.

---

## Editing a project's retention

A project's retention can be changed from Settings at any time, and the change
applies to data that is already stored, not just to future events:

- shortening the window deletes that project's events which are now outside it,
  and moves every surviving event onto the new window;
- lengthening it deletes nothing and moves the surviving expiries out.

Without that reconciliation a shortened window would be a lie: each event
carries the expiry it was stamped with when it arrived, so going from 90 to 7
days would have left week-old data sitting for another 83 days.

The API key is saved before the response goes out, so what the next ingest
stamps is correct immediately; the cleanup of the backlog runs in the
background and is logged. Lowering the value asks for confirmation in the
console, since it deletes data.

A project name can have more than one key (prod, staging, dev). Those keys share
one event namespace, so a change is reconciled to the longest window any of them
still asks for: shortening one key can never delete data a sibling key promised
to keep.

---

## Where events are stored

By default raw events live in PostgreSQL, in the `events` table. That table is
the one part of the system that grows without bound — millions of rows a day —
so it can be moved to an S3-compatible object store instead (Backblaze B2,
MinIO, AWS S3):

```
APP_EVENT_STORE=s3
APP_S3_ENDPOINT=https://s3.us-east-005.backblazeb2.com
APP_S3_REGION=us-east-005
APP_S3_BUCKET=your-bucket
APP_S3_ACCESS_KEY_ID=...
APP_S3_SECRET_ACCESS_KEY=...
```

PostgreSQL is still required and is still the system of record: users, API keys
and the pre-aggregated rollup tables that every chart reads stay there. Only
the raw stream moves, and in `s3` mode the `events` table is not written at
all — which is what frees the disk.

The rollup worker rebuilds the hourly buckets by streaming each hour back out
of the bucket and aggregating in Go, so every dashboard panel, top-routes list
and CSV export keeps working unchanged. Raw-event responses report which store
answered them, and any request can override the choice with
`?store=s3|postgres|clickhouse`.

With the events in a bucket, `APP_RETENTION_DAYS=0` — the natural setting when
storage is cheap and the point is to stop losing history to a full disk —
means **keep forever**: nothing expires, and the retention pass does nothing.
The same value with the events in Postgres means the table grows without bound,
and the service says so at startup.

Full details, including the cost trade-offs, the migration runbook, how to
reclaim the disk from the old `events` table (including the TimescaleDB
hypertable trap), and the reverting path:
[`docs/object-store.md`](docs/object-store.md).

### Rollups are pruned separately

`APP_RETENTION_DAYS` governs **raw events only**. The pre-aggregated tables are
not events, and nothing pruned them until two knobs existed for it:
`APP_ROLLUP_RETENTION_DAYS` (hourly) and `APP_DAILY_ROLLUP_RETENTION_DAYS`
(daily), both defaulting to `0` = keep forever.

That gap is not academic. One deployment reached **185 GB of rollups behind a
70 GB events table** — clearing the events freed less than a third of the disk.

The route-level tables are large because `route` is the full request path
including its query string, which is deliberate: it is what lets you search a
route by a real value, and what lets the dashboard show what people are
watching rather than `/media`. The cardinality cost is inherited by
`route_buckets` (~10-15M rows/day) and is bounded with retention, not by
stripping the stored route — that change was made and reverted.

Measurements, the retention knobs, the disk-reclaim procedures, and what
actually makes Top endpoints slow (and what does not fix it):
[`docs/rollups.md`](docs/rollups.md).

---

## Development

### Prerequisites

- Go 1.23 or later

### Building

```bash
go build ./...
```

### Running Locally

```bash
go run main.go

# Or
CGO_ENABLED=0 go run main.go

# To build a local binary:
CGO_ENABLED=0 go build -o apiinsight .
```

### Code Quality

```bash
go fmt ./... && go vet ./...
npx prettier --write "web/**/*.{html,css,js,css}"
```

---

## License

See [LICENSE](LICENSE) file.
