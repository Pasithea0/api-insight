package db

import (
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"apiinsight/internal/config"
)

// These tests exercise the real TimescaleDB path and therefore need a real
// server. Point them at a throwaway database:
//
//	APIINSIGHT_TEST_DATABASE_URL='postgres://user:pass@localhost:55434/apiinsight_test?sslmode=disable' \
//	  go test ./internal/db/ -run Timescale -v -count=1
//
// Without that variable they skip, so the default `go test ./...` stays
// hermetic on machines with no database.
//
// NEVER point this at production: it converts the events table to a
// hypertable and runs the retention worker, both of which rewrite data.

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dsn := os.Getenv("APIINSIGHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set APIINSIGHT_TEST_DATABASE_URL to run TimescaleDB integration tests")
	}
	return &config.Config{
		DatabaseURL:                   dsn,
		StatementTimeoutMs:            120000,
		RetentionDays:                 30,
		TimescaleEnabled:              true,
		TimescaleChunkInterval:        24 * time.Hour,
		TimescaleColumnstoreAfter:     0, // don't compress: keeps assertions simple
		TimescaleContinuousAggregates: true,
	}
}

// cleanupProject removes every trace of a test project so the suite is
// re-runnable even after an earlier run failed part-way through.
//
// Deleting raw events is not enough on its own: the hourly continuous
// aggregate keeps its materialised rows, so the cagg windows the test cares
// about are refreshed after the deletes. Otherwise a stale cagg row leaks
// into the next run's totals (which is exactly how this helper was found).
func cleanupProject(t *testing.T, gdb *gorm.DB, project string, caggWindows []time.Time) {
	t.Helper()
	for _, model := range []interface{}{
		&Event{}, &MetricBucket{}, &RouteBucket{}, &DailyMetricBucket{},
		&DailyRouteBucket{}, &AttributeKeyIndex{},
	} {
		if err := gdb.Where("project = ?", project).Delete(model).Error; err != nil {
			t.Fatalf("cleanup %T: %v", model, err)
		}
	}
	for _, w := range caggWindows {
		if err := refreshRollupAggregates(gdb, w, w.Add(time.Hour)); err != nil {
			t.Fatalf("cleanup cagg refresh: %v", err)
		}
	}
}

// TestTimescaleHypertableSetup proves the conversion actually happens and
// that the events table ends up hypertable-compatible.
func TestTimescaleHypertableSetup(t *testing.T) {
	gdb, err := Connect(testConfig(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if !isHypertable(gdb, "events") {
		t.Fatal("events was not converted to a hypertable")
	}

	st := TimescaleStatus()
	if !st.Available {
		t.Fatal("timescaledb extension not detected as available")
	}
	t.Logf("timescaledb version=%s chunks=%d caggs=%t", st.Version, st.ChunkCount, st.ContinuousAggregates)
	if !st.ContinuousAggregates {
		t.Error("continuous aggregates were not created")
	}

	// TimescaleDB requires the partitioning column in the primary key, so
	// events_pkey must be composite. If this regresses, create_hypertable
	// would fail on the next attempt.
	var pkCols []string
	if err := gdb.Raw(`
		SELECT a.attname
		FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = 'events'::regclass AND i.indisprimary
		ORDER BY a.attname
	`).Scan(&pkCols).Error; err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	if len(pkCols) != 2 || pkCols[0] != "created_at" || pkCols[1] != "id" {
		t.Fatalf("events primary key = %v, want [created_at id]", pkCols)
	}
}

// TestRetentionDoesNotDeleteLiveRows is the regression test for the ctid
// bug: on a hypertable, `WHERE ctid IN (...)` matches the same physical
// address in every chunk, so the old retention statement deleted rows that
// had not expired. Retention must be keyed on id, which is unique across
// the whole hypertable.
//
// The two rows are placed in different chunks (3 days apart, with a 1-day
// chunk interval) precisely so the cross-chunk aliasing would be exercised.
func TestRetentionDoesNotDeleteLiveRows(t *testing.T) {
	gdb, err := Connect(testConfig(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	project := "retention-regression-test"
	now := time.Now().UTC()
	cleanupProject(t, gdb, project, nil)

	// 10 rows already expired, 3 days old (their own chunk).
	expiredAt := now.Add(-time.Hour)
	// 10 rows still live, 1 day old (a different chunk).
	liveUntil := now.Add(30 * 24 * time.Hour)

	var rows []Event
	for i := 0; i < 10; i++ {
		rows = append(rows,
			Event{
				CreatedAt:  now.Add(-72 * time.Hour),
				ExpiresAt:  &expiredAt,
				UserID:     "retention-user",
				Project:    project,
				Route:      "/expired",
				Method:     "GET",
				Status:     200,
				DurationMs: 1,
			},
			Event{
				CreatedAt:  now.Add(-24 * time.Hour),
				ExpiresAt:  &liveUntil,
				UserID:     "retention-user",
				Project:    project,
				Route:      "/live",
				Method:     "GET",
				Status:     200,
				DurationMs: 1,
			},
		)
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	countByRoute := func(route string) int64 {
		var n int64
		if err := gdb.Model(&Event{}).
			Where("project = ? AND route = ?", project, route).
			Count(&n).Error; err != nil {
			t.Fatalf("count %s: %v", route, err)
		}
		return n
	}
	if got := countByRoute("/expired"); got != 10 {
		t.Fatalf("seeded expired rows = %d, want 10", got)
	}

	if err := runRetentionOnce(gdb); err != nil {
		t.Fatalf("runRetentionOnce: %v", err)
	}

	expiredLeft := countByRoute("/expired")
	liveLeft := countByRoute("/live")

	if liveLeft != 10 {
		t.Errorf("retention deleted %d LIVE rows (want 0) — ctid-style cross-chunk deletion is back", 10-liveLeft)
	}
	if expiredLeft != 0 {
		t.Errorf("expired rows remaining = %d, want 0", expiredLeft)
	}
	t.Logf("after retention: expired=%d live=%d (live must be 10)", expiredLeft, liveLeft)

	// Clean up so the test can be re-run.
	if err := gdb.Where("project = ?", project).Delete(&Event{}).Error; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestRollupUsesContinuousAggregate verifies the rollup worker's cagg path
// produces the same numbers as computing directly from raw events. Two
// different code paths agreeing is the property that matters: it proves the
// pre-computed percentiles (percentile_cont inside a continuous aggregate)
// are not silently wrong.
func TestRollupUsesContinuousAggregate(t *testing.T) {
	gdb, err := Connect(testConfig(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !UseContinuousAggregates() {
		t.Skip("continuous aggregates unavailable in this environment")
	}

	project := "rollup-cagg-test"
	user := "rollup-user"
	// A completed hour, comfortably in the past.
	hourStart := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	cleanupProject(t, gdb, project, []time.Time{hourStart})

	var rows []Event
	for i := 1; i <= 100; i++ {
		rows = append(rows, Event{
			CreatedAt:  hourStart.Add(time.Duration(i) * time.Second),
			UserID:     user,
			Project:    project,
			Route:      "/rollup",
			Method:     "GET",
			Status:     200,
			DurationMs: int64(i),
		})
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runAggregationOnce(gdb, hourStart); err != nil {
		t.Fatalf("runAggregationOnce: %v", err)
	}

	var bucket MetricBucket
	if err := gdb.Where("user_id = ? AND project = ? AND bucket_start = ?", user, project, hourStart).
		First(&bucket).Error; err != nil {
		t.Fatalf("read metric bucket: %v", err)
	}
	if bucket.TotalCount != 100 {
		t.Errorf("total_count = %d, want 100", bucket.TotalCount)
	}

	// Ground truth straight from the raw hypertable.
	var want struct {
		P50, P95, P99 float64
	}
	if err := gdb.Raw(`
		SELECT
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms) AS p50,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95,
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) AS p99
		FROM events
		WHERE user_id = ? AND project = ? AND created_at >= ? AND created_at < ?
	`, user, project, hourStart, hourStart.Add(time.Hour)).Scan(&want).Error; err != nil {
		t.Fatalf("ground truth: %v", err)
	}

	if diff := int64(want.P50) - bucket.DurationP50Ms; diff > 1 || diff < -1 {
		t.Errorf("p50 from rollup = %d, direct = %d", bucket.DurationP50Ms, int64(want.P50))
	}
	if diff := int64(want.P95) - bucket.DurationP95Ms; diff > 1 || diff < -1 {
		t.Errorf("p95 from rollup = %d, direct = %d", bucket.DurationP95Ms, int64(want.P95))
	}
	t.Logf("rollup p50=%d p95=%d p99=%d vs direct p50=%d p95=%d p99=%d",
		bucket.DurationP50Ms, bucket.DurationP95Ms, bucket.DurationP99Ms,
		int64(want.P50), int64(want.P95), int64(want.P99))

	// The route rollup must also have been produced from the cagg.
	var routeBuckets int64
	if err := gdb.Model(&RouteBucket{}).
		Where("user_id = ? AND project = ? AND bucket_start = ?", user, project, hourStart).
		Count(&routeBuckets).Error; err != nil {
		t.Fatalf("count route buckets: %v", err)
	}
	if routeBuckets == 0 {
		t.Error("no route bucket written from the routes continuous aggregate")
	}

	cleanupProject(t, gdb, project, []time.Time{hourStart})
}

// TestPlainPostgresStillWorks guards the "no TimescaleDB available" path:
// with the feature disabled, Connect must succeed and leave events as an
// ordinary table.
func TestPlainPostgresStillWorks(t *testing.T) {
	cfg := testConfig(t)
	cfg.TimescaleEnabled = false

	gdb, err := Connect(cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := TimescaleStatus(); got.Available || got.IsHypertable {
		t.Errorf("Timescale state should be empty when disabled, got %+v", got)
	}
	// The table must still be usable.
	if err := gdb.Create(&Event{
		CreatedAt:  time.Now().UTC(),
		UserID:     "plain-user",
		Project:    "plain-postgres-test",
		Route:      "/plain",
		Method:     "GET",
		DurationMs: 5,
	}).Error; err != nil {
		t.Fatalf("insert on plain postgres: %v", err)
	}
	if err := gdb.Where("project = ?", "plain-postgres-test").Delete(&Event{}).Error; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
