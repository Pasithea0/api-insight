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
```

### Code Quality

```bash
go fmt ./... && go vet ./...
npx prettier --write "web/**/*.{html,css,js,css}"
```

---

## License

See [LICENSE](LICENSE) file.
