package db

import "testing"

// TestRollupPlanAppliesTheRightWindowToTheRightTable guards the mapping from a
// configuration knob to the tables it governs. Getting it wrong is silent
// until somebody notices charts that are missing, which is why it is pinned
// here rather than left to the query.
func TestRollupPlanAppliesTheRightWindowToTheRightTable(t *testing.T) {
	plan := rollupPlan(RollupWindows{HourlyDays: 90, DailyDays: 0})

	got := map[string]int{}
	for _, p := range plan {
		got[p.table] = p.days
	}

	if got["route_buckets"] != 90 || got["metric_buckets"] != 90 {
		t.Errorf("hourly tables did not get the hourly window: %v", got)
	}
	// Daily history must survive an hourly-only policy: the dashboard reads it
	// for every range beyond 48h.
	if _, present := got["daily_route_buckets"]; present {
		t.Errorf("daily_route_buckets was pruned under an hourly-only window: %v", got)
	}
	if _, present := got["daily_metric_buckets"]; present {
		t.Errorf("daily_metric_buckets was pruned under an hourly-only window: %v", got)
	}
	if len(plan) != 2 {
		t.Errorf("plan has %d entries, want 2: %v", len(plan), plan)
	}
}

// TestRollupPlanZeroMeansKeepForever: 0 is the default for both knobs, so it
// has to mean "prune nothing" rather than "prune everything older than zero
// days", which would delete every rollup the moment the service started.
func TestRollupPlanZeroMeansKeepForever(t *testing.T) {
	if plan := rollupPlan(RollupWindows{}); len(plan) != 0 {
		t.Errorf("default windows planned %d prunes, want 0: %v", len(plan), plan)
	}
	if plan := rollupPlan(RollupWindows{HourlyDays: 0, DailyDays: 0}); len(plan) != 0 {
		t.Errorf("explicit zeros planned %d prunes, want 0: %v", len(plan), plan)
	}
}

// TestRollupPlanNegativeIsIgnoredRatherThanInverted: a negative window would
// compute a cutoff in the future and delete live rows.
func TestRollupPlanNegativeIsIgnoredRatherThanInverted(t *testing.T) {
	plan := rollupPlan(RollupWindows{HourlyDays: -5, DailyDays: -10})
	if len(plan) != 0 {
		t.Errorf("negative windows planned %d prunes, want 0: %v", len(plan), plan)
	}
}

// TestRollupPlanCoversEveryRollupTable: the plan is built from rollupTables, so
// this fails if a table is added to the writer without being added there.
func TestRollupPlanCoversEveryRollupTable(t *testing.T) {
	plan := rollupPlan(RollupWindows{HourlyDays: 30, DailyDays: 30})
	if len(plan) != len(rollupTables) {
		t.Fatalf("plan covers %d tables, want all %d", len(plan), len(rollupTables))
	}

	byName := map[string]string{}
	for _, p := range plan {
		byName[p.table] = p.ageCol
	}

	// The age column has to be the one the table actually keeps its date in,
	// or the delete matches nothing and the table grows in silence.
	want := map[string]string{
		"route_buckets":        "bucket_start",
		"metric_buckets":       "bucket_start",
		"daily_route_buckets":  "bucket_date",
		"daily_metric_buckets": "bucket_date",
	}
	for table, col := range want {
		if got, ok := byName[table]; !ok {
			t.Errorf("%s is not in the plan", table)
		} else if got != col {
			t.Errorf("%s pruned on %q, want %q", table, got, col)
		}
	}
}
