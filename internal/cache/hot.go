package cache

import (
	"sort"
	"sync"
	"time"

	dbpkg "apiinsight/internal/db"
)

// maxHotEvents caps how many events the hot source keeps for the current
// hour. It matches the SQL partial-hour query's cap, so a panel that reads
// either source sees the same amount of history.
const maxHotEvents = 5000

// HotSource accumulates the current hour's aggregates from the ingest
// stream.
//
// It exists because the partial-hour cache used to be filled by querying the
// Postgres events table, and when the raw stream lives in an object store
// there is nothing there to query — whereas re-downloading the current hour
// from the bucket every 30 seconds would be both slow and, on B2, billed.
// The events are already in memory on their way to the writer, so the
// aggregates the dashboard needs for "so far this hour" are computed from
// the batch as it passes.
//
// The trade-off is explicit: this is process-local state. On restart the
// current hour starts empty, so startup primes it by scanning the current
// hour back out of the store once (see main.go). After that it is live.
type HotSource struct {
	mu   sync.RWMutex
	hour time.Time
	data *PartialHourData
}

// NewHotSource returns an empty accumulator.
func NewHotSource() *HotSource {
	return &HotSource{}
}

// Add folds a batch of ingested events into the current hour's aggregates.
// Events outside the current hour are ignored: a backdated event belongs to
// an hour the rollup worker owns, not to the live view.
func (h *HotSource) Add(events []dbpkg.Event) {
	if h == nil || len(events) == 0 {
		return
	}
	now := time.Now().UTC()
	hourStart := now.Truncate(time.Hour)
	hourEnd := hourStart.Add(time.Hour)

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.data == nil || !h.hour.Equal(hourStart) {
		h.hour = hourStart
		h.data = emptyPartialHour(now)
	}

	data := h.data
	for i := range events {
		ev := events[i]
		if ev.CreatedAt.Before(hourStart) || !ev.CreatedAt.Before(hourEnd) {
			continue
		}

		upKey := ev.UserID + "|" + ev.Project
		data.TrafficBy30[upKey+"|"+formatEpoch30(ev.CreatedAt.UTC().Unix()/1800)]++
		data.TotalCount[upKey]++
		data.DurationSum[upKey] += float64(ev.DurationMs)
		if ev.Status >= 400 {
			data.ErrorCount[upKey]++
		}

		rKey := upKey + "|" + ev.Route
		data.RouteCount[rKey]++
		data.RouteDurSum[rKey] += float64(ev.DurationMs)
		if ev.Status >= 400 {
			data.RouteError[rKey]++
		}

		data.Events = append(data.Events, EventPHData{
			ID:         ev.ID,
			UserID:     ev.UserID,
			Project:    ev.Project,
			Route:      ev.Route,
			Method:     ev.Method,
			Status:     ev.Status,
			DurationMs: ev.DurationMs,
			CreatedAt:  ev.CreatedAt,
		})
	}

	// Arrival order is chronological in practice but a batch may carry a
	// backdated event, so the list is trimmed by timestamp rather than by
	// position.
	if len(data.Events) > maxHotEvents*2 {
		sort.SliceStable(data.Events, func(i, j int) bool {
			return data.Events[i].CreatedAt.After(data.Events[j].CreatedAt)
		})
		data.Events = data.Events[:maxHotEvents]
	}
}

// PartialHour implements PartialHourProvider. It returns a deep copy: the
// accumulator is mutated by the ingest path, and handing a caller a live map
// would be a data race waiting for a dashboard load to hit it.
func (h *HotSource) PartialHour(now time.Time) *PartialHourData {
	if h == nil {
		return emptyPartialHour(now)
	}
	hourStart := now.UTC().Truncate(time.Hour)

	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.data == nil || !h.hour.Equal(hourStart) {
		return emptyPartialHour(now)
	}
	return h.data.clone(now)
}

// emptyPartialHour builds an empty snapshot with every map allocated, so
// callers never have to nil-check.
func emptyPartialHour(now time.Time) *PartialHourData {
	return &PartialHourData{
		TrafficBy30: make(map[string]int64),
		TotalCount:  make(map[string]int64),
		ErrorCount:  make(map[string]int64),
		DurationSum: make(map[string]float64),
		RouteCount:  make(map[string]int64),
		RouteError:  make(map[string]int64),
		RouteDurSum: make(map[string]float64),
		RefreshedAt: now,
	}
}

// clone deep-copies the snapshot, newest-first and capped, matching what the
// SQL partial-hour query returns.
func (d *PartialHourData) clone(now time.Time) *PartialHourData {
	out := &PartialHourData{
		TrafficBy30: make(map[string]int64, len(d.TrafficBy30)),
		TotalCount:  make(map[string]int64, len(d.TotalCount)),
		ErrorCount:  make(map[string]int64, len(d.ErrorCount)),
		DurationSum: make(map[string]float64, len(d.DurationSum)),
		RouteCount:  make(map[string]int64, len(d.RouteCount)),
		RouteError:  make(map[string]int64, len(d.RouteError)),
		RouteDurSum: make(map[string]float64, len(d.RouteDurSum)),
		RefreshedAt: now,
	}
	for k, v := range d.TrafficBy30 {
		out.TrafficBy30[k] = v
	}
	for k, v := range d.TotalCount {
		out.TotalCount[k] = v
	}
	for k, v := range d.ErrorCount {
		out.ErrorCount[k] = v
	}
	for k, v := range d.DurationSum {
		out.DurationSum[k] = v
	}
	for k, v := range d.RouteCount {
		out.RouteCount[k] = v
	}
	for k, v := range d.RouteError {
		out.RouteError[k] = v
	}
	for k, v := range d.RouteDurSum {
		out.RouteDurSum[k] = v
	}

	if len(d.Events) > 0 {
		events := append([]EventPHData(nil), d.Events...)
		sort.SliceStable(events, func(i, j int) bool {
			return events[i].CreatedAt.After(events[j].CreatedAt)
		})
		if len(events) > maxHotEvents {
			events = events[:maxHotEvents]
		}
		out.Events = events
	}
	return out
}
