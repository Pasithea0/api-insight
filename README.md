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
