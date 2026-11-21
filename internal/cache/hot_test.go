package cache

import (
	"testing"
	"time"

	dbpkg "apiinsight/internal/db"
)

func hotEvent(id uint, user, project, route string, status int, dur int64, at time.Time) dbpkg.Event {
	return dbpkg.Event{
		ID:         id,
		UserID:     user,
		Project:    project,
		Route:      route,
		Method:     "GET",
		Status:     status,
		DurationMs: dur,
		CreatedAt:  at,
	}
}

// TestHotSourceAccumulatesTheCurrentHour is the contract the partial-hour
// cache depends on: the same aggregate shapes the SQL partial-hour queries
// produced, computed from the ingest stream instead of from a table.
func TestHotSourceAccumulatesTheCurrentHour(t *testing.T) {
	hot := NewHotSource()
	now := time.Now().UTC()

	hot.Add([]dbpkg.Event{
		hotEvent(1, "7", "p", "/a", 200, 100, now),
		hotEvent(2, "7", "p", "/a", 500, 300, now),
		hotEvent(3, "7", "p", "/b", 200, 200, now),
		hotEvent(4, "9", "q", "/c", 200, 50, now),
	})

	data := hot.PartialHour(now)
	if data == nil {
		t.Fatal("PartialHour returned nil")
	}

	if got := data.TotalCount["7|p"]; got != 3 {
		t.Errorf("TotalCount[7|p] = %d, want 3", got)
	}
	if got := data.ErrorCount["7|p"]; got != 1 {
		t.Errorf("ErrorCount[7|p] = %d, want 1", got)
	}
	// 100 + 300 + 200
	if got := data.DurationSum["7|p"]; got != 600 {
		t.Errorf("DurationSum[7|p] = %v, want 600", got)
	}
	if got := data.RouteCount["7|p|/a"]; got != 2 {
		t.Errorf("RouteCount[7|p|/a] = %d, want 2", got)
	}
	if got := data.RouteError["7|p|/a"]; got != 1 {
		t.Errorf("RouteError[7|p|/a] = %d, want 1", got)
	}

	// The key format has to match what the traffic panel looks up, which is
	// the 30-minute floor rendered by formatEpoch30.
	key := "7|p|" + formatEpoch30(now.Unix()/1800)
	if got := data.TrafficBy30[key]; got != 3 {
		t.Errorf("TrafficBy30[%s] = %d, want 3 (keys present: %v)", key, got, keysOf(data.TrafficBy30))
	}

	if got := data.TotalCount["9|q"]; got != 1 {
		t.Errorf("a second tenant was not aggregated separately: %v", data.TotalCount)
	}
	if len(data.Events) != 4 {
		t.Errorf("got %d recent events, want 4", len(data.Events))
	}
}

func keysOf(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestHotSourceIgnoresEventsOutsideTheHour is what keeps a backdated batch
// from being double-counted: it belongs to an hour the rollup worker owns.
func TestHotSourceIgnoresEventsOutsideTheHour(t *testing.T) {
	hot := NewHotSource()
	now := time.Now().UTC()

	hot.Add([]dbpkg.Event{
		hotEvent(1, "7", "p", "/old", 200, 10, now.Add(-3*time.Hour)),
		hotEvent(2, "7", "p", "/newer", 200, 10, now.Add(time.Hour)),
	})

	data := hot.PartialHour(now)
	if len(data.TotalCount) != 0 {
		t.Errorf("events outside the hour were counted: %v", data.TotalCount)
	}
	if len(data.Events) != 0 {
		t.Errorf("events outside the hour were listed: %v", data.Events)
	}
}

// TestHotSourceRollsOverOnANewHour covers the transition: the live view must
// start empty for a new hour rather than carrying the previous one's totals
// forward, which would double-count once the rollup for that hour lands.
func TestHotSourceRollsOverOnANewHour(t *testing.T) {
	hot := NewHotSource()
	now := time.Now().UTC()

	hot.Add([]dbpkg.Event{hotEvent(1, "7", "p", "/a", 200, 10, now)})
	if got := hot.PartialHour(now).TotalCount["7|p"]; got != 1 {
		t.Fatalf("setup: TotalCount = %d, want 1", got)
	}

	nextHour := hot.PartialHour(now.Add(2 * time.Hour))
	if len(nextHour.TotalCount) != 0 {
		t.Errorf("a new hour inherited %v", nextHour.TotalCount)
	}
}

// TestHotSourceSnapshotIsIsolated is a race guard: the accumulator is written
// by the ingest path while a dashboard request is reading a snapshot, so the
// snapshot must own its maps.
func TestHotSourceSnapshotIsIsolated(t *testing.T) {
	hot := NewHotSource()
	now := time.Now().UTC()
	hot.Add([]dbpkg.Event{hotEvent(1, "7", "p", "/a", 200, 10, now)})

	snapshot := hot.PartialHour(now)

	// Mutating what was handed out must not corrupt the accumulator.
	snapshot.TotalCount["7|p"] = 999
	snapshot.TrafficBy30["bogus"] = 999

	again := hot.PartialHour(now)
	if got := again.TotalCount["7|p"]; got != 1 {
		t.Errorf("the caller's mutation leaked into the accumulator: TotalCount = %d", got)
	}
	if _, ok := again.TrafficBy30["bogus"]; ok {
		t.Error("the caller's mutation leaked into the accumulator's traffic map")
	}
}

// TestPartialHourCacheFromProviderAlwaysReportsFresh matters because callers
// read a false from HasRecentData as "fall back to a raw database query", and
// in object-store mode there is no query to fall back to.
func TestPartialHourCacheFromProviderAlwaysReportsFresh(t *testing.T) {
	hot := NewHotSource()
	hot.Add([]dbpkg.Event{hotEvent(1, "7", "p", "/a", 200, 10, time.Now().UTC())})

	c := NewPartialHourCacheFromProvider(hot, time.Hour)
	defer c.Stop()

	if !c.HasRecentData() {
		t.Error("a provider-fed cache must always report recent data")
	}
	if got := c.Get().TotalCount["7|p"]; got != 1 {
		t.Errorf("the cache did not pick up the provider's data: %v", c.Get().TotalCount)
	}
}
