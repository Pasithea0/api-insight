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

	CreatedAt time.Time

	// ExpiresAt is the timestamp after which this event is eligible
	// for deletion by the retention worker. A nil value means the
	// event does not currently expire.
	ExpiresAt *time.Time `gorm:"index"`

	// Owner of this event (will later map to a user/tenant).
	UserID string `gorm:"index"`

	Project string `gorm:"index"`
	Route   string `gorm:"index"`
	Method  string `gorm:"index"`
	Status  int

	DurationMs int64
	RemoteIP   string

	// Attributes holds arbitrary key/value pairs for this event, so
	// callers can attach custom metrics (e.g. price, plan, region)
	// without schema changes. This will back flexible charts later.
	Attributes datatypes.JSONMap `gorm:"type:json"`
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
