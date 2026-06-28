package db

import (
	"time"

	"gorm.io/datatypes"
)

// Event represents a single API request event as stored in SQLite.
// The schema is intentionally compact but flexible and can evolve as
// the product grows.
type Event struct {
	ID uint `gorm:"primaryKey"`

	CreatedAt time.Time `gorm:"index:idx_events_user_created_at,priority:2;index:idx_events_user_project_created_at,priority:3;index:idx_events_user_status_created_at,priority:3"`

	// ExpiresAt is the timestamp after which this event is eligible
	// for deletion by the retention worker. A nil value means the
	// event does not currently expire.
	ExpiresAt *time.Time `gorm:"index"`

	// Owner of this event (will later map to a user/tenant).
	UserID string `gorm:"index;index:idx_events_user_created_at,priority:1;index:idx_events_user_project_created_at,priority:1;index:idx_events_user_status_created_at,priority:1"`

	Project string `gorm:"index;index:idx_events_user_project_created_at,priority:2"`
	Route   string `gorm:"index"`
	Method  string `gorm:"index"`
	Status  int    `gorm:"index;index:idx_events_user_status_created_at,priority:2"`

	DurationMs int64
	RemoteIP   string

	// Attributes holds arbitrary key/value pairs for this event, so
	// callers can attach custom metrics (e.g. price, plan, region)
	// without schema changes. This will back flexible charts later.
	Attributes datatypes.JSONMap `gorm:"type:jsonb;index:,type:gin"`
}

// MetricBucket stores pre-aggregated hourly metrics per (user, project)
// for fast error-rate and latency-percentile charts. Filled by the
// aggregation worker.
type MetricBucket struct {
	ID uint `gorm:"primaryKey"`

	UserID      string    `gorm:"uniqueIndex:idx_metric_bucket_unique,priority:1;not null"`
	Project     string    `gorm:"uniqueIndex:idx_metric_bucket_unique,priority:2;not null"`
	BucketStart time.Time `gorm:"uniqueIndex:idx_metric_bucket_unique,priority:3;not null"` // start of the hour (UTC)

	TotalCount    int64 `gorm:"not null"` // total requests in this hour
	ErrorCount    int64 `gorm:"not null"` // requests with status >= 400
	DurationP50Ms int64 `gorm:"not null"` // 50th percentile duration ms
	DurationP95Ms int64 `gorm:"not null"` // 95th percentile duration ms
	DurationP99Ms int64 `gorm:"not null"` // 99th percentile duration ms
}

// RouteBucket stores pre-aggregated hourly route-level stats per (user, project, route)
// for fast top-routes and avg-duration queries. Filled by the aggregation worker.
type RouteBucket struct {
	ID uint `gorm:"primaryKey"`

	UserID      string    `gorm:"uniqueIndex:idx_route_bucket_unique,priority:1;not null"`
	Project     string    `gorm:"uniqueIndex:idx_route_bucket_unique,priority:2;not null"`
	Route       string    `gorm:"uniqueIndex:idx_route_bucket_unique,priority:3;not null"`
	BucketStart time.Time `gorm:"uniqueIndex:idx_route_bucket_unique,priority:4;not null"`

	TotalCount    int64            `gorm:"not null"`
	ErrorCount    int64            `gorm:"not null"`
	AvgDurationMs float64          `gorm:"not null"`
	StatusCounts  datatypes.JSONMap `gorm:"type:jsonb"`
}

// DailyMetricBucket stores pre-aggregated daily metrics per (user, project)
// for fast long-range queries (7d, 14d, 30d+). Filled by the aggregation worker
// at midnight UTC. The dashboard switches to these when range > 48h.
type DailyMetricBucket struct {
	ID uint `gorm:"primaryKey"`

	UserID     string    `gorm:"uniqueIndex:idx_daily_metric_unique,priority:1;not null"`
	Project    string    `gorm:"uniqueIndex:idx_daily_metric_unique,priority:2;not null"`
	BucketDate time.Time `gorm:"uniqueIndex:idx_daily_metric_unique,priority:3;not null"` // start of the day (UTC, truncated to midnight)

	TotalCount    int64 `gorm:"not null"` // total requests this day
	ErrorCount    int64 `gorm:"not null"` // requests with status >= 400
	DurationP50Ms int64 `gorm:"not null"` // 50th percentile duration ms
	DurationP95Ms int64 `gorm:"not null"` // 95th percentile duration ms
	DurationP99Ms int64 `gorm:"not null"` // 99th percentile duration ms
}

// DailyRouteBucket stores pre-aggregated daily route-level stats.
// Enables fast route-level queries across long time ranges.
type DailyRouteBucket struct {
	ID uint `gorm:"primaryKey"`

	UserID     string    `gorm:"uniqueIndex:idx_daily_route_unique,priority:1;not null"`
	Project    string    `gorm:"uniqueIndex:idx_daily_route_unique,priority:2;not null"`
	Route      string    `gorm:"uniqueIndex:idx_daily_route_unique,priority:3;not null"`
	BucketDate time.Time `gorm:"uniqueIndex:idx_daily_route_unique,priority:4;not null"`

	TotalCount    int64            `gorm:"not null"`
	ErrorCount    int64            `gorm:"not null"`
	AvgDurationMs float64          `gorm:"not null"`
	StatusCounts  datatypes.JSONMap `gorm:"type:jsonb"`
}

// AttributeKeyIndex caches distinct attribute keys per (user, project)
// to avoid expensive jsonb_object_keys scans on the full events table.
type AttributeKeyIndex struct {
	ID      uint   `gorm:"primaryKey"`
	UserID  string `gorm:"uniqueIndex:idx_attr_key_unique,priority:1;not null"`
	Project string `gorm:"uniqueIndex:idx_attr_key_unique,priority:2;not null"`
	Key     string `gorm:"uniqueIndex:idx_attr_key_unique,priority:3;not null"`
}
