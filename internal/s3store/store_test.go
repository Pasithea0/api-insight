package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"

	dbpkg "apiinsight/internal/db"
)

// memStore is an in-memory ObjectStore. The store logic — key layout,
// buffering, filtering, ordering, retention — is what these tests exercise,
// and none of it needs a live bucket to be meaningful.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte

	putErr error
	gets   int
	lists  int
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	// Copy: the caller reuses its buffer.
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objects[key] = cp
	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	data, ok := m.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return data, nil
}

func (m *memStore) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

func (m *memStore) List(_ context.Context, prefix string, fn func(key string) error) error {
	m.mu.Lock()
	m.lists++
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()

	// Sorted so a test failure is reproducible rather than map-order random.
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// ---------------------------------------------------------------------------
// IDs
// ---------------------------------------------------------------------------

// TestIDGeneratorIsUniqueWithinAMillisecond is the case that breaks a naive
// timestamp-only id: the ingest handler reads the clock once per request, so
// a 5000-event batch shares one timestamp.
func TestIDGeneratorIsUniqueWithinAMillisecond(t *testing.T) {
	gen := NewIDGenerator()
	at := time.Now().UTC()

	seen := make(map[uint]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := gen.Next(at)
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d at iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}
}

// TestIDGeneratorIsMonotonic covers a backwards clock step (NTP): ids must
// keep increasing, because the recent-events ordering tie-breaks on them.
func TestIDGeneratorIsMonotonic(t *testing.T) {
	gen := NewIDGenerator()
	at := time.Now().UTC()

	prev := gen.Next(at)
	for i := 0; i < 100; i++ {
		// Deliberately going backwards.
		next := gen.Next(at.Add(-time.Duration(i+1) * time.Minute))
		if next <= prev {
			t.Fatalf("id went backwards: %d then %d", prev, next)
		}
		prev = next
	}
}

// TestIDSurvivesJSON is the reason the id is 53 bits rather than a 63-bit
// snowflake. It is the exact failure a dashboard would hit: the id is put in
// a URL, and JavaScript's JSON parser cannot represent a larger integer.
func TestIDSurvivesJSON(t *testing.T) {
	gen := NewIDGenerator()
	id := gen.Next(time.Now().UTC())

	if id >= 1<<53 {
		t.Fatalf("id %d exceeds JavaScript's exact integer range (2^53)", id)
	}

	body, err := json.Marshal(map[string]any{"id": id})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(body, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// float64 is what any JSON consumer sees, including Go's own
	// interface{} decoding — the worst case for precision loss.
	if got := uint(roundTripped["id"].(float64)); got != id {
		t.Fatalf("id lost precision over JSON: %d became %d", id, got)
	}
}

// TestIDHourRoundTrip is the property EventByID depends on: the hour an event
// is stored under must be recoverable from its id alone, because an object
// store has no index to look it up in.
func TestIDHourRoundTrip(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 21, 14, 59, 59, 999999999, time.UTC),
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		// A fresh generator per case: the generator is monotonic by design,
		// so asking one instance for a backwards timestamp would correctly
		// keep counting forward and not re-embed the earlier time.
		gen := NewIDGenerator()
		id := gen.Next(at)
		hour, ok := IDHour(id)
		if !ok {
			t.Fatalf("IDHour(%d) reported no timestamp", id)
		}
		if want := at.Truncate(time.Hour); !hour.Equal(want) {
			t.Errorf("IDHour = %s, want %s", hour.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

// TestIDIsNeverZeroForBackfill covers the trap that a naive
// "milliseconds since epoch" id falls into: an event dated before the epoch
// yields exactly 0, and 0 is the sentinel this codebase uses everywhere for
// "this event has no id yet", so the writer would keep re-assigning it.
func TestIDIsNeverZeroForBackfill(t *testing.T) {
	gen := NewIDGenerator()
	for _, at := range []time.Time{
		time.Date(2019, 5, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Now().UTC(),
	} {
		if id := gen.Next(at); id == 0 {
			t.Fatalf("Next(%s) returned the unassigned sentinel 0", at)
		}
	}
}

func TestIDTimeRejectsForeignIDs(t *testing.T) {
	// Ints a previous Postgres deployment handed out must be rejected so the
	// resolver falls through to Postgres instead of listing a bogus hour
	// bucket. The marker bit is what makes this exact.
	for _, id := range []uint{0, 1, 123456, 1_000_000_000_000} {
		if _, ok := IDTime(id); ok {
			t.Errorf("IDTime(%d) claimed a timestamp", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Keys and codec
// ---------------------------------------------------------------------------

func TestObjectKeyLayout(t *testing.T) {
	hour := time.Date(2026, 11, 21, 14, 37, 12, 0, time.UTC)
	key := objectKey("events", hour, 42)

	if want := "events/dt=2026-11-21/hour=14/000000000042.ndjson.gz"; key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	got, ok := parseObjectHour("events", key)
	if !ok {
		t.Fatalf("parseObjectHour refused its own key %q", key)
	}
	if !got.Equal(hour.Truncate(time.Hour)) {
		t.Errorf("parsed hour = %s, want %s", got, hour.Truncate(time.Hour))
	}
}

func TestParseObjectHourRejectsNonsense(t *testing.T) {
	for _, key := range []string{
		"events/README",
		"events/dt=2026-11-21/oops/x.ndjson.gz",
		"events/dt=not-a-date/hour=99/x.ndjson.gz",
	} {
		if _, ok := parseObjectHour("events", key); ok {
			t.Errorf("parseObjectHour(%q) accepted a malformed key", key)
		}
	}
}

func TestHoursBetweenIsNewestFirstAndCapped(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 15, 0, 0, time.UTC)
	end := time.Date(2026, 11, 21, 13, 45, 0, 0, time.UTC)

	hours := hoursBetween(start, end, 0)
	if len(hours) != 4 {
		t.Fatalf("got %d hours, want 4: %v", len(hours), hours)
	}
	for i := 1; i < len(hours); i++ {
		if !hours[i].Before(hours[i-1]) {
			t.Fatalf("hours are not newest-first: %v", hours)
		}
	}
	if got := hoursBetween(start, end, 2); len(got) != 2 {
		t.Errorf("cap ignored: got %d hours", len(got))
	}
	if got := hoursBetween(end, start, 0); got != nil {
		t.Errorf("inverted range should be empty, got %v", got)
	}
}

func TestCodecRoundTrip(t *testing.T) {
	expires := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	in := []dbpkg.Event{
		{
			ID:         idMarker | 12345,
			CreatedAt:  time.Date(2026, 11, 21, 14, 1, 2, 0, time.UTC),
			ExpiresAt:  &expires,
			UserID:     "7",
			Project:    "payments-api",
			Route:      "/api/orders?page=2",
			Method:     "POST",
			Status:     500,
			DurationMs: 1234,
			RemoteIP:   "192.0.2.1",
			Attributes: datatypes.JSONMap{"region": "us-east-1", "retries": 3},
		},
		{ID: 12346, CreatedAt: time.Now().UTC(), Route: "/health"},
	}

	data, err := encodeEvents(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeEvents(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d events, want %d", len(out), len(in))
	}

	got := out[0]
	if got.ID != in[0].ID || got.UserID != in[0].UserID || got.Project != in[0].Project {
		t.Errorf("identity fields did not survive: %+v", got)
	}
	if got.Route != in[0].Route {
		t.Errorf("route = %q, want %q (the full request path must be preserved)", got.Route, in[0].Route)
	}
	if !got.CreatedAt.Equal(in[0].CreatedAt) {
		t.Errorf("created_at = %s, want %s", got.CreatedAt, in[0].CreatedAt)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %s", got.ExpiresAt, expires)
	}
	if got.Attributes["region"] != "us-east-1" {
		t.Errorf("attribute did not survive: %v", got.Attributes)
	}
	// JSON numbers come back as float64; 3 must still render as 3.
	if v, ok := got.Attributes["retries"].(float64); !ok || v != 3 {
		t.Errorf("numeric attribute = %v (%T), want 3", got.Attributes["retries"], got.Attributes["retries"])
	}
	if out[1].ExpiresAt != nil {
		t.Errorf("a nil expiry became %v", out[1].ExpiresAt)
	}
}

// ---------------------------------------------------------------------------
// Writer + Reader
// ---------------------------------------------------------------------------

// fixedRetention is a ProjectRetentionLookup that answers one window.
type fixedRetention struct {
	days    int
	maxDays int
}

func (f fixedRetention) ProjectRetentionDays(_, _ string) int {
	if f.days <= 0 {
		return 0
	}
	return f.days
}
func (f fixedRetention) DefaultRetentionDays() int { return f.days }
func (f fixedRetention) MaxRetentionDays() int {
	if f.maxDays > 0 {
		return f.maxDays
	}
	return f.days
}

// newTestStore wires a Store over a memStore and returns both, so a test can
// write, flush and read without a bucket or a background flusher.
func newTestStore(t *testing.T, opts Options) (*Store, *memStore) {
	t.Helper()
	mem := newMemStore()
	opts.Prefix = "events"
	// A long interval keeps the background flusher out of the way; tests
	// call flush explicitly so the assertions are not racing a ticker.
	opts.FlushInterval = time.Hour
	store := New(mem, opts)
	t.Cleanup(func() { _ = store.Close() })
	return store, mem
}

func eventAt(at time.Time, project string, status int, dur int64, route string) dbpkg.Event {
	return dbpkg.Event{
		CreatedAt:  at.UTC(),
		UserID:     "7",
		Project:    project,
		Route:      route,
		Method:     "GET",
		Status:     status,
		DurationMs: dur,
		Attributes: datatypes.JSONMap{"env": "prod"},
	}
}

func TestWriterGroupsByHourAndAssignsIDs(t *testing.T) {
	store, mem := newTestStore(t, Options{})

	hourA := time.Date(2026, 11, 21, 14, 5, 0, 0, time.UTC)
	hourB := time.Date(2026, 11, 21, 15, 5, 0, 0, time.UTC)

	batch := []dbpkg.Event{
		eventAt(hourA, "p", 200, 10, "/a"),
		eventAt(hourA.Add(time.Minute), "p", 200, 10, "/b"),
		eventAt(hourB, "p", 200, 10, "/c"),
	}
	// The ingest path leaves ids zero; the writer is the authority.
	if err := store.InsertEvents(context.Background(), batch); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.writer.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := mem.count(); got != 2 {
		t.Fatalf("got %d objects, want 2 (one per hour bucket): %v", got, mem.keys())
	}
	for _, k := range mem.keys() {
		if !strings.HasSuffix(k, ".ndjson.gz") {
			t.Errorf("object %q has an unexpected name", k)
		}
	}
	for i, ev := range batch {
		if ev.ID == 0 {
			t.Errorf("event %d was stored without an id", i)
		}
	}
}

// TestWriterKeepsBufferedOnFailure is the durability property: a bucket
// outage must not silently discard events.
func TestWriterKeepsBufferedOnFailure(t *testing.T) {
	store, mem := newTestStore(t, Options{MaxBufferEvents: 100})
	mem.putErr = errors.New("bucket unavailable")

	if err := store.InsertEvents(context.Background(), []dbpkg.Event{
		eventAt(time.Now().UTC(), "p", 200, 10, "/a"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.writer.flush(context.Background()); err == nil {
		t.Fatal("expected flush to report the failure")
	}
	if buffered, _ := store.Pending(); buffered != 1 {
		t.Fatalf("buffered %d events, want 1 kept for retry", buffered)
	}

	mem.putErr = nil
	if err := store.writer.flush(context.Background()); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if buffered, _ := store.Pending(); buffered != 0 {
		t.Fatalf("buffered %d events after a successful retry, want 0", buffered)
	}
	if got := mem.count(); got != 1 {
		t.Fatalf("got %d objects after retry, want 1", got)
	}
}

func seedStore(t *testing.T, store *Store, events []dbpkg.Event) {
	t.Helper()
	if err := store.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	if err := store.writer.flush(context.Background()); err != nil {
		t.Fatalf("seed flush: %v", err)
	}
}

// TestQueryEventsIsNewestFirstAndPaged covers the ordering contract the
// dashboard depends on, including the hour-bucket boundary: events are
// written unsorted and spread over hours, and must come back strictly
// newest-first with limit/offset applied after the sort.
func TestQueryEventsIsNewestFirstAndPaged(t *testing.T) {
	store, _ := newTestStore(t, Options{})

	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var events []dbpkg.Event
	for i := 0; i < 10; i++ {
		// Deliberately shuffled hours so insertion order is not time order.
		at := base.Add(time.Duration((i*7)%5) * time.Hour).Add(time.Duration(i) * time.Minute)
		events = append(events, eventAt(at, "p", 200, int64(i), fmt.Sprintf("/r%d", i)))
	}
	seedStore(t, store, events)

	got, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		UserID: "7",
		Cutoff: base.Add(-time.Hour),
		Limit:  5,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.After(got[i-1].CreatedAt) {
			t.Fatalf("results are not newest-first: %s then %s",
				got[i-1].CreatedAt, got[i].CreatedAt)
		}
	}

	page2, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		UserID: "7",
		Cutoff: base.Add(-time.Hour),
		Limit:  5,
		Offset: 5,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 5 {
		t.Fatalf("page 2 returned %d events, want 5", len(page2))
	}
	if page2[0].CreatedAt.After(got[len(got)-1].CreatedAt) {
		t.Error("page 2 overlaps page 1")
	}
}

func TestQueryEventsAppliesFilters(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	events := []dbpkg.Event{
		eventAt(base.Add(1*time.Minute), "payments", 200, 5, "/ok"),
		eventAt(base.Add(2*time.Minute), "payments", 500, 5, "/boom"),
		eventAt(base.Add(3*time.Minute), "search", 200, 5, "/ok"),
	}
	events[0].Attributes = datatypes.JSONMap{"env": "prod"}
	events[1].Attributes = datatypes.JSONMap{"env": "dev"}
	events[2].Attributes = datatypes.JSONMap{"env": "prod"}
	seedStore(t, store, events)

	tests := []struct {
		name   string
		query  dbpkg.EventQuery
		wantNo int
	}{
		{name: "project", query: dbpkg.EventQuery{Project: "payments"}, wantNo: 2},
		{name: "error status", query: dbpkg.EventQuery{Status: "error"}, wantNo: 1},
		{name: "success status", query: dbpkg.EventQuery{Status: "success"}, wantNo: 2},
		{name: "attribute equal", query: dbpkg.EventQuery{AttrKey: "env", AttrValue: "prod"}, wantNo: 2},
		{name: "search on route", query: dbpkg.EventQuery{Field: "route", Pattern: "%boom%"}, wantNo: 1},
		{name: "combined", query: dbpkg.EventQuery{Project: "payments", AttrKey: "env", AttrValue: "dev"}, wantNo: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.query
			q.Cutoff = base.Add(-time.Hour)
			q.Limit = 100
			got, err := store.QueryEvents(context.Background(), q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(got) != tc.wantNo {
				t.Fatalf("got %d events, want %d", len(got), tc.wantNo)
			}
		})
	}
}

// TestEventByIDFindsByDerivedHour is the whole point of embedding the
// timestamp in the id.
func TestEventByIDFindsByDerivedHour(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	at := time.Date(2026, 11, 21, 16, 30, 0, 0, time.UTC)

	batch := []dbpkg.Event{eventAt(at, "p", 201, 42, "/orders")}
	seedStore(t, store, batch)

	ev := batch[0]
	if ev.ID == 0 {
		t.Fatal("writer did not assign an id")
	}

	got, err := store.EventByID(context.Background(), strconv.Itoa(int(ev.ID)))
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if got == nil {
		t.Fatal("EventByID returned nil for an event that exists")
	}
	if got.Route != "/orders" || got.ID != ev.ID {
		t.Errorf("got %+v, want the seeded event", got)
	}

	// An id from a previous Postgres deployment must miss cleanly, so the
	// resolver can fall through to Postgres.
	foreign, err := store.EventByID(context.Background(), "123456")
	if err != nil {
		t.Fatalf("EventByID(foreign): %v", err)
	}
	if foreign != nil {
		t.Errorf("a foreign id resolved to %+v, want nil", foreign)
	}
}

func TestScanEventsCoversTheRangeInclusively(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	seedStore(t, store, []dbpkg.Event{
		eventAt(base, "p", 200, 1, "/at-start"),
		eventAt(base.Add(time.Hour), "p", 200, 1, "/at-end"),
		eventAt(base.Add(2*time.Hour), "p", 200, 1, "/outside"),
	})

	var routes []string
	err := store.ScanEvents(context.Background(), base, base.Add(time.Hour), dbpkg.ScanFilter{}, func(ev dbpkg.Event) error {
		routes = append(routes, ev.Route)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	sort.Strings(routes)
	want := []string{"/at-end", "/at-start"}
	if len(routes) != len(want) || routes[0] != want[0] || routes[1] != want[1] {
		t.Fatalf("scanned %v, want %v (empty hours must not hide neighbours)", routes, want)
	}
}

// TestRetentionFiltersAtReadTime is how per-project retention works on a
// store whose objects cannot be rewritten: the window is applied as the data
// is read, so a shortened window takes effect on the next request.
func TestRetentionFiltersAtReadTime(t *testing.T) {
	store, _ := newTestStore(t, Options{
		Retention: fixedRetention{days: 2, maxDays: 2},
	})

	now := time.Now().UTC()
	seedStore(t, store, []dbpkg.Event{
		eventAt(now.Add(-10*time.Minute), "p", 200, 1, "/fresh"),
		eventAt(now.Add(-70*time.Hour), "p", 200, 1, "/stale"),
	})

	got, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		Cutoff: now.Add(-7 * 24 * time.Hour),
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Route != "/fresh" {
		t.Fatalf("got %d events (%v), want only /fresh", len(got), got)
	}
}

// TestRetentionUnlimitedKeepsEverything is the object-store half of
// APP_RETENTION_DAYS=0: with no window to resolve, the reader must not drop a
// single event, however old.
func TestRetentionUnlimitedKeepsEverything(t *testing.T) {
	store, _ := newTestStore(t, Options{
		Retention: fixedRetention{days: 0, maxDays: 0},
	})

	now := time.Now().UTC()
	seedStore(t, store, []dbpkg.Event{
		eventAt(now.Add(-10*time.Minute), "p", 200, 1, "/fresh"),
		eventAt(now.Add(-70*time.Hour), "p", 200, 1, "/old"),
		eventAt(now.Add(-90*24*time.Hour), "p", 200, 1, "/ancient"),
	})

	got, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		// A cutoff far enough back that nothing is excluded by the query
		// itself, so only the retention policy can drop rows.
		Cutoff: now.Add(-365 * 24 * time.Hour),
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 3 {
		routes := make([]string, 0, len(got))
		for _, e := range got {
			routes = append(routes, e.Route)
		}
		t.Fatalf("unlimited retention returned %d of 3 events: %v", len(got), routes)
	}
}

// TestDeleteBeforeKeepsWholeHours is the safety property of the retention
// pass: an hour bucket is dropped only when the whole bucket is outside the
// window, so a partial hour never disappears.
func TestDeleteBeforeKeepsWholeHours(t *testing.T) {
	store, mem := newTestStore(t, Options{})

	now := time.Now().UTC().Truncate(time.Hour)
	seedStore(t, store, []dbpkg.Event{
		eventAt(now.Add(-3*time.Hour), "p", 200, 1, "/old"),
		eventAt(now.Add(-time.Hour), "p", 200, 1, "/recent"),
	})
	before := mem.count()
	if before != 2 {
		t.Fatalf("setup wrote %d objects, want 2", before)
	}

	// Cutoff in the middle of the old hour: that bucket is NOT entirely
	// outside the window, so nothing may be deleted.
	if _, err := store.DeleteBefore(context.Background(), now.Add(-3*time.Hour).Add(30*time.Minute)); err != nil {
		t.Fatalf("DeleteBefore: %v", err)
	}
	if mem.count() != before {
		t.Fatalf("a partially-expired hour bucket was deleted (%d objects left, want %d)", mem.count(), before)
	}

	// Cutoff at the start of the recent hour: the old bucket is now wholly
	// outside and may go.
	removed, err := store.DeleteBefore(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeleteBefore: %v", err)
	}
	if removed != 1 || mem.count() != 1 {
		t.Fatalf("removed %d objects, %d left; want 1 removed, 1 left", removed, mem.count())
	}
}

// TestStreamHourSkipsVanishedObjects covers the race between a read and the
// retention pass: a key that is listed and then deleted must be skipped, not
// surfaced as a 500. The lister fabricates the key, which is exactly what a
// listing-then-delete race looks like from the reader's side.
func TestStreamHourSkipsVanishedObjects(t *testing.T) {
	store, mem := newTestStore(t, Options{})
	hour := time.Now().UTC().Truncate(time.Hour)
	seedStore(t, store, []dbpkg.Event{eventAt(hour.Add(time.Minute), "p", 200, 1, "/survivor")})

	phantom := &phantomLister{
		ObjectStore: mem,
		phantomKey:  objectKey("events", hour, 424242),
	}
	reader := NewReader(phantom, ReaderOptions{Prefix: "events"})

	var routes []string
	err := reader.ScanEvents(context.Background(), hour, hour.Add(time.Hour), dbpkg.ScanFilter{}, func(ev dbpkg.Event) error {
		routes = append(routes, ev.Route)
		return nil
	})
	if err != nil {
		t.Fatalf("a vanished object failed the whole scan: %v", err)
	}
	if len(routes) != 1 || routes[0] != "/survivor" {
		t.Fatalf("got %v, want [/survivor]", routes)
	}
}

// phantomLister advertises one extra key that does not exist, so a test can
// exercise the listing/read race deterministically.
type phantomLister struct {
	ObjectStore
	phantomKey string
}

func (p *phantomLister) List(ctx context.Context, prefix string, fn func(key string) error) error {
	found := p.phantomKey == ""
	err := p.ObjectStore.List(ctx, prefix, func(key string) error {
		if key == p.phantomKey {
			found = true
		}
		return fn(key)
	})
	if err != nil {
		return err
	}
	if !found {
		return fn(p.phantomKey)
	}
	return nil
}

// TestQueryEventsTakesTheNewestWithinABusyHour is a regression test.
//
// An earlier version of QueryEvents stopped the walk as soon as it had
// `offset+limit` rows, which could land in the middle of an hour bucket. Object
// keys carry no intra-hour time order, so the rows it kept were an arbitrary
// subset of that hour rather than its newest — a page that looked plausible
// and was wrong. Every event here is in one hour, so only an implementation
// that finishes the hour can return the right answer.
func TestQueryEventsTakesTheNewestWithinABusyHour(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	hour := time.Now().UTC().Truncate(time.Hour)

	var events []dbpkg.Event
	const total = 400
	for i := 0; i < total; i++ {
		events = append(events, eventAt(hour.Add(time.Duration(i)*time.Second), "p", 200, 1, fmt.Sprintf("/r%03d", i)))
	}
	seedStore(t, store, events)

	got, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		Cutoff: hour,
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
	for i, want := range []string{"/r399", "/r398", "/r397", "/r396", "/r395"} {
		if got[i].Route != want {
			t.Errorf("position %d = %s, want %s (the newest events must win)", i, got[i].Route, want)
		}
	}

	// And paging must not overlap.
	page2, err := store.QueryEvents(context.Background(), dbpkg.EventQuery{
		Cutoff: hour,
		Limit:  5,
		Offset: 5,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 5 || page2[0].Route != "/r394" {
		t.Fatalf("page 2 starts at %v, want /r394", page2)
	}
}

// TestReaderNeverListsWithoutAPrefix guards the cost invariant: B2 bills per
// list, and an empty prefix would enumerate the whole bucket.
func TestReaderNeverListsWithoutAPrefix(t *testing.T) {
	store, mem := newTestStore(t, Options{})
	seedStore(t, store, []dbpkg.Event{eventAt(time.Now().UTC().Truncate(time.Hour).Add(time.Minute), "p", 200, 1, "/a")})

	prefixes := map[string]bool{}
	listingStore := &recordingLister{ObjectStore: mem, prefixes: prefixes}
	reader := NewReader(listingStore, ReaderOptions{Prefix: "events"})

	if _, _, err := reader.ObjectCount(context.Background()); err != nil {
		t.Fatalf("ObjectCount: %v", err)
	}
	if _, err := reader.QueryEvents(context.Background(), dbpkg.EventQuery{Cutoff: time.Now().Add(-time.Hour), Limit: 5}); err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(prefixes) == 0 {
		t.Fatal("no listing happened at all")
	}
	for p := range prefixes {
		if strings.TrimSpace(p) == "" {
			t.Fatal("an unbucketed listing was issued")
		}
	}
}

type recordingLister struct {
	ObjectStore
	prefixes map[string]bool
}

func (r *recordingLister) List(ctx context.Context, prefix string, fn func(key string) error) error {
	r.prefixes[prefix] = true
	return r.ObjectStore.List(ctx, prefix, fn)
}
