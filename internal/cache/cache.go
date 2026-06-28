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
type PartialHourCache struct {
	mu          sync.RWMutex
	db          *gorm.DB
	data        *PartialHourData
	lastRefresh time.Time
	stopCh      chan struct{}
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
	RouteCount    map[string]int64
	RouteError    map[string]int64
	RouteDurSum   map[string]float64

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
	c := &PartialHourCache{
		db:     db,
		data:   &PartialHourData{},
		stopCh: make(chan struct{}),
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
		UserID    string
		Project   string
		Epoch30   int64
		Total     int64
		Errors    int64
		DurSum    float64
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
		UserID    string
		Project   string
		Route     string
		Status    int
		Total     int64
		DurSum    float64
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
func (c *PartialHourCache) HasRecentData() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.lastRefresh) < 35*time.Second
}