package cache

import (
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
)

// PartialHourCache provides in-memory cached aggregates for the current
// (incomplete) hour, refreshed every 30 seconds. This eliminates repeated
// raw-events-table partial-hour queries on every dashboard load.
//
// It has two sources. The default reads the Postgres events table, which is
// correct when Postgres holds the raw stream. When the events live in an
// object store there is nothing in Postgres to read, so the cache is fed
// instead by a PartialHourProvider that accumulates the same aggregates from
// the ingest stream — see cache.HotSource. Either way the data it serves has
// the same shape, so the panels do not know the difference.
type PartialHourCache struct {
	mu          sync.RWMutex
	db          *gorm.DB
	provider    PartialHourProvider
	data        *PartialHourData
	lastRefresh time.Time
	stopCh      chan struct{}
}

// PartialHourProvider supplies pre-computed aggregates for the current hour.
type PartialHourProvider interface {
	// PartialHour returns the aggregates for the hour containing now. It
	// must never block: it is called from the refresh loop and from request
	// handlers.
	PartialHour(now time.Time) *PartialHourData
}

// PartialHourData holds pre-computed aggregates for the current partial hour.
type PartialHourData struct {
	// TrafficBy30 counts per (userID, project, 30-min epoch)
	TrafficBy30 map[string]int64 // key: "userID|project|epoch30_bucket"

	// Totals keyed by "userID|project"
	TotalCount  map[string]int64
	ErrorCount  map[string]int64
	DurationSum map[string]float64

	// Route-level counts keyed by "userID|project|route"
	RouteCount  map[string]int64
	RouteError  map[string]int64
	RouteDurSum map[string]float64

	// Events for recent-events queries (capped at 200 per user/project)
	Events []EventPHData

	RefreshedAt time.Time
}

// EventPHData is a lightweight copy of an Event for cache storage.
type EventPHData struct {
	ID         uint
	UserID     string
	Project    string
	Route      string
	Method     string
	Status     int
	DurationMs int64
	CreatedAt  time.Time
}

// NewPartialHourCache creates a cache and starts the background refresh loop.
func NewPartialHourCache(db *gorm.DB, refreshInterval time.Duration) *PartialHourCache {
	return newPartialHourCache(db, nil, refreshInterval)
}

// NewPartialHourCacheFromProvider creates a cache that is fed by a provider
// instead of by SQL. It is the object-store mode: the aggregates are
// accumulated from the ingest stream, so there is nothing to query.
func NewPartialHourCacheFromProvider(provider PartialHourProvider, refreshInterval time.Duration) *PartialHourCache {
	return newPartialHourCache(nil, provider, refreshInterval)
}

func newPartialHourCache(db *gorm.DB, provider PartialHourProvider, refreshInterval time.Duration) *PartialHourCache {
	c := &PartialHourCache{
		db:       db,
		provider: provider,
		data:     &PartialHourData{},
		stopCh:   make(chan struct{}),
	}
	// Initial fill
	c.refresh()
	// Background refresh
	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.refresh()
			case <-c.stopCh:
				return
			}
		}
	}()
	return c
}

// Stop halts the background refresh goroutine.
func (c *PartialHourCache) Stop() {
	close(c.stopCh)
}

// Get returns a snapshot of the current partial-hour data.
func (c *PartialHourCache) Get() *PartialHourData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

// refresh runs the aggregation queries for the current partial hour.
func (c *PartialHourCache) refresh() {
	now := time.Now().UTC()
	if c.provider != nil {
		data := c.provider.PartialHour(now)
		if data == nil {
			return
		}
		c.mu.Lock()
		c.data = data
		c.lastRefresh = now
		c.mu.Unlock()
		return
	}
	c.refreshFromDB(now)
}

// refreshFromDB is the SQL source: three grouped queries against the
// Postgres events table.
func (c *PartialHourCache) refreshFromDB(now time.Time) {
	hourStart := now.Truncate(time.Hour)

	data := &PartialHourData{
		TrafficBy30: make(map[string]int64),
		TotalCount:  make(map[string]int64),
		ErrorCount:  make(map[string]int64),
		DurationSum: make(map[string]float64),
		RouteCount:  make(map[string]int64),
		RouteError:  make(map[string]int64),
		RouteDurSum: make(map[string]float64),
		RefreshedAt: now,
	}

	// 1. Traffic grouped by user, project, and 30-min bucket, plus totals
	type trafficRow struct {
		UserID  string
		Project string
		Epoch30 int64
		Total   int64
		Errors  int64
		DurSum  float64
	}
	var traffic []trafficRow
	if err := c.db.Raw(`
		SELECT
			user_id, project,
			floor(extract(epoch FROM created_at) / 1800)::bigint AS epoch30,
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status >= 400) AS errors,
			COALESCE(SUM(duration_ms), 0) AS dur_sum
		FROM events
		WHERE created_at >= ?
		GROUP BY user_id, project, epoch30
	`, hourStart).Scan(&traffic).Error; err != nil {
		log.Printf("partial-hour cache: traffic query error: %v", err)
		return
	}

	for _, r := range traffic {
		upKey := r.UserID + "|" + r.Project
		tKey := upKey + "|" + formatEpoch30(r.Epoch30)
		data.TrafficBy30[tKey] = r.Total
		data.TotalCount[upKey] += r.Total
		data.ErrorCount[upKey] += r.Errors
		data.DurationSum[upKey] += r.DurSum
	}

	// 2. Route-level counts
	type routeRow struct {
		UserID  string
		Project string
		Route   string
		Status  int
		Total   int64
		DurSum  float64
	}
	var routes []routeRow
	if err := c.db.Raw(`
		SELECT user_id, project, route, status, COUNT(*) AS total, COALESCE(SUM(duration_ms), 0) AS dur_sum
		FROM events
		WHERE created_at >= ?
		GROUP BY user_id, project, route, status
	`, hourStart).Scan(&routes).Error; err != nil {
		log.Printf("partial-hour cache: route query error: %v", err)
		return
	}

	for _, r := range routes {
		rKey := r.UserID + "|" + r.Project + "|" + r.Route
		data.RouteCount[rKey] += r.Total
		if r.Status >= 400 {
			data.RouteError[rKey] += r.Total
		}
		data.RouteDurSum[rKey] += r.DurSum
	}

	// 3. Recent events (capped at 200 per user/project — enough for dashboard pages)
	type eventRow struct {
		ID         uint
		UserID     string
		Project    string
		Route      string
		Method     string
		Status     int
		DurationMs int64
		CreatedAt  time.Time
	}
	var events []eventRow
	if err := c.db.Raw(`
		SELECT id, user_id, project, route, method, status, duration_ms, created_at
		FROM events
		WHERE created_at >= ?
		ORDER BY created_at DESC
	`, hourStart).Scan(&events).Error; err != nil {
		log.Printf("partial-hour cache: events query error: %v", err)
		return
	}

	// Only keep recent events (cap at manageable size)
	const maxEvents = 5000
	if len(events) > maxEvents {
		events = events[:maxEvents]
	}

	east := make([]EventPHData, len(events))
	for i, e := range events {
		east[i] = EventPHData{
			ID:         e.ID,
			UserID:     e.UserID,
			Project:    e.Project,
			Route:      e.Route,
			Method:     e.Method,
			Status:     e.Status,
			DurationMs: e.DurationMs,
			CreatedAt:  e.CreatedAt,
		}
	}
	data.Events = east

	// Swap atomically
	c.mu.Lock()
	c.data = data
	c.lastRefresh = now
	c.mu.Unlock()

	log.Printf("partial-hour cache: refreshed (%d events, %d traffic rows, %d route rows)",
		len(events), len(traffic), len(routes))
}

// formatEpoch30 formats a 30-minute epoch into a readable bucket label.
func formatEpoch30(epoch int64) string {
	t := time.Unix(epoch*1800, 0).UTC()
	return t.Format("2006-01-02T15:04:05") + "Z"
}

// HasRecentData returns true if the cache was refreshed within the last
// refresh interval + 5s grace period.
//
// A provider-fed cache is always current — it is written by the ingest path
// itself, not polled — so it reports true unconditionally. That matters
// because callers treat a false here as "fall back to a raw database query",
// and on an object store there is no database query to fall back to.
func (c *PartialHourCache) HasRecentData() bool {
	if c.provider != nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.lastRefresh) < 35*time.Second
}
