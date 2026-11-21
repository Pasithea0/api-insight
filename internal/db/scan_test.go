package db

import (
	"math"
	"testing"
	"time"

	"gorm.io/datatypes"
)

// TestCompileLikeSemantics pins the LIKE translation against the behaviour
// the SQL stores exhibit, because a scan filter that matched something
// different from Postgres would make `?store=` comparisons meaningless.
func TestCompileLikeSemantics(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		{`%orders%`, `/api/orders/1`, true},
		{`%orders%`, `/api/ORDERS/1`, false}, // LIKE is case-sensitive
		{`%orders%`, `/api/order/1`, false},
		{`/api/%`, `/api/orders`, true},
		{`/api/%`, `x/api/orders`, false}, // anchored at both ends
		{`%orders`, `/api/orders`, true},
		{`%orders`, `/api/orders/1`, false},
		{`/a_c`, `/abc`, true},   // _ is exactly one character
		{`/a_c`, `/ac`, false},   // ... so not zero
		{`/a_c`, `/abbc`, false}, // ... and not two
		{`100\%`, `100%`, true},  // backslash escapes the wildcard
		{`100\%`, `1000`, false},
		{`%`, ``, true},        // % matches the empty string
		{`a_b`, `a\nb`, false}, // _ is one character, not a newline pair
		{`%a%b%`, `xaybz`, true},
		{`%a%b%`, `xba`, false}, // the order of the literals matters
	}

	for _, tc := range tests {
		re, err := compileLike(tc.pattern)
		if err != nil {
			t.Fatalf("compileLike(%q): %v", tc.pattern, err)
		}
		if got := re.match(tc.subject); got != tc.want {
			t.Errorf("LIKE %q against %q = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func baseEvent() Event {
	return Event{
		ID:         1,
		CreatedAt:  time.Now().UTC(),
		UserID:     "7",
		Project:    "payments",
		Route:      "/api/orders",
		Method:     "POST",
		Status:     500,
		DurationMs: 25,
		RemoteIP:   "192.0.2.9",
		Attributes: datatypes.JSONMap{"env": "prod", "attempts": 2},
	}
}

func TestMatcherFilters(t *testing.T) {
	ev := baseEvent()

	tests := []struct {
		name   string
		filter ScanFilter
		want   bool
	}{
		{"empty filter accepts everything", ScanFilter{}, true},
		{"matching user", ScanFilter{UserID: "7"}, true},
		{"other user", ScanFilter{UserID: "8"}, false},
		{"matching project", ScanFilter{Project: "payments"}, true},
		{"error status matches a 500", ScanFilter{Status: "error"}, true},
		{"success status rejects a 500", ScanFilter{Status: "success"}, false},
		{"string attribute matches", ScanFilter{AttrKey: "env", AttrValue: "prod"}, true},
		{"different attribute value", ScanFilter{AttrKey: "env", AttrValue: "dev"}, false},

		// This is the faithful-to-Postgres case. The SQL path filters with
		// `attributes @> json_build_object(key, value)`, and jsonb
		// containment is type-sensitive, so a text filter value does NOT
		// match a stored number. Matching it here would make the object
		// store and Postgres answer the same question differently.
		{"numeric attribute is not matched by a string filter", ScanFilter{AttrKey: "attempts", AttrValue: "2"}, false},

		{"attribute presence", ScanFilter{AttrExists: "env"}, true},
		{"missing attribute presence", ScanFilter{AttrExists: "region"}, false},

		{"search on route", ScanFilter{Field: "route", Pattern: "%orders%"}, true},
		{"search on route, no match", ScanFilter{Field: "route", Pattern: "%invoices%"}, false},
		{"path is an alias of route", ScanFilter{Field: "path", Pattern: "%orders%"}, true},
		{"search on method", ScanFilter{Field: "method", Pattern: "POST"}, true},
		{"search on status text", ScanFilter{Field: "status", Pattern: "5%"}, true},
		{"search on an attribute", ScanFilter{Field: "env", Pattern: "%rod%"}, true},

		// A LIKE against a missing attribute is NULL in SQL, i.e. not a
		// match — not an error, and not a match against "".
		{"search on an absent attribute never matches", ScanFilter{Field: "region", Pattern: "%"}, false},

		{"combined filters", ScanFilter{UserID: "7", Project: "payments", Status: "error", AttrKey: "env", AttrValue: "prod"}, true},
		{"combined filters, one wrong", ScanFilter{UserID: "7", Project: "search", Status: "error"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewMatcher(tc.filter)
			if err != nil {
				t.Fatalf("NewMatcher: %v", err)
			}
			if got := m.Match(ev); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFromEventQueryCarriesTheFilters(t *testing.T) {
	q := EventQuery{
		UserID:    "7",
		Project:   "payments",
		Status:    "error",
		AttrKey:   "env",
		AttrValue: "prod",
		Field:     "route",
		Pattern:   "%orders%",
	}
	f := FromEventQuery(q)

	if f.UserID != q.UserID || f.Project != q.Project || f.Status != q.Status {
		t.Errorf("scalar filters lost: %+v", f)
	}
	if f.AttrKey != q.AttrKey || f.AttrValue != q.AttrValue {
		t.Errorf("attribute filter lost: %+v", f)
	}
	if f.Field != q.Field || f.Pattern != q.Pattern {
		t.Errorf("search filter lost: %+v", f)
	}
	if f.IsEmpty() {
		t.Error("a populated query produced an empty filter")
	}
	if !(ScanFilter{}).IsEmpty() {
		t.Error("the zero filter should report empty")
	}
}

// TestPercentileContMatchesPostgres checks the interpolation against values
// Postgres percentile_cont returns, since the object-store rollup has to
// produce the same stored numbers as the SQL rollup.
func TestPercentileContMatchesPostgres(t *testing.T) {
	sorted := []int64{1, 2, 3, 4}

	// Postgres: percentile_cont(0.5) WITHIN GROUP (ORDER BY x) over 1..4 = 2.5
	if got := percentileCont(sorted, 0.5); got != 2.5 {
		t.Errorf("p50 = %v, want 2.5", got)
	}
	// rank = 0.95 * 3 = 2.85 -> 3 + 0.85*(4-3). Compared with a tolerance
	// because the interpolation is done in floating point.
	if got := percentileCont(sorted, 0.95); math.Abs(got-3.85) > 1e-9 {
		t.Errorf("p95 = %v, want 3.85", got)
	}
	// Degenerate cases must not divide by zero.
	if got := percentileCont([]int64{}, 0.5); got != 0 {
		t.Errorf("empty p50 = %v, want 0", got)
	}
	if got := percentileCont([]int64{7}, 0.99); got != 7 {
		t.Errorf("single-value p99 = %v, want 7", got)
	}
}

func TestPercentilesOfDoesNotMutateItsInput(t *testing.T) {
	durations := []int64{5, 1, 4, 2, 3}
	p50, _, _ := percentilesOf(durations)

	if p50 != 3 {
		t.Errorf("p50 = %d, want 3", p50)
	}
	// The caller may still need the slice in its original order.
	want := []int64{5, 1, 4, 2, 3}
	for i := range want {
		if durations[i] != want[i] {
			t.Fatalf("percentilesOf reordered its input: %v", durations)
		}
	}
}

func TestSplitMetricKey(t *testing.T) {
	// Project names are operator-supplied and can contain anything except a
	// "|", which is what makes splitting on the first one unambiguous.
	user, project := splitMetricKey("7|payments-api", nil)
	if user != "7" || project != "payments-api" {
		t.Errorf("got (%q, %q), want (7, payments-api)", user, project)
	}
	user, project = splitMetricKey("no-separator", nil)
	if user != "no-separator" || project != "" {
		t.Errorf("got (%q, %q), want the whole key and an empty project", user, project)
	}
}

// TestMergeRouteStatusRowsIsTheSharedArithmetic is the reason both rollup
// drivers funnel through one merge: the weighted average a chart reads must
// not depend on which store produced it.
func TestMergeRouteStatusRowsIsTheSharedArithmetic(t *testing.T) {
	rows := []routeAggRow{
		{UserID: "7", Project: "p", Route: "/a", Status: 200, Count: 3, AvgDuration: 100},
		{UserID: "7", Project: "p", Route: "/a", Status: 500, Count: 1, AvgDuration: 400},
		{UserID: "7", Project: "p", Route: "/b", Status: 200, Count: 2, AvgDuration: 50},
	}

	merged := mergeRouteStatusRows(rows)
	if len(merged) != 2 {
		t.Fatalf("got %d groups, want 2", len(merged))
	}

	a := merged[routeBucketKey{UserID: "7", Project: "p", Route: "/a"}]
	if a == nil {
		t.Fatal("route /a is missing")
	}
	if a.TotalCount != 4 || a.ErrorCount != 1 {
		t.Errorf("counts = %d total / %d error, want 4/1", a.TotalCount, a.ErrorCount)
	}
	// 3*100 + 1*400 = 700 over 4 events.
	if a.DurSum != 700 {
		t.Errorf("dur sum = %v, want 700", a.DurSum)
	}
	if a.StatusCnt[200] != 3 || a.StatusCnt[500] != 1 {
		t.Errorf("status histogram = %v", a.StatusCnt)
	}

	// And it renders into the bucket row a chart reads.
	bucket := routeBucketKey{UserID: "7", Project: "p", Route: "/a"}.bucket(a, time.Unix(0, 0).UTC())
	if bucket.AvgDurationMs != 175 {
		t.Errorf("avg duration = %v, want 175", bucket.AvgDurationMs)
	}
	if bucket.StatusCounts["200"] != int64(3) {
		t.Errorf("status_counts = %v, want a 200 key of 3", bucket.StatusCounts)
	}
}
