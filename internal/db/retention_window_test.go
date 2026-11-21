package db

import "testing"

func TestRetentionWider(t *testing.T) {
	// 0 means unlimited, so it is wider than everything — including, and this
	// is the point, wider than any finite window.
	tests := []struct {
		a, b int
		want bool
		why  string
	}{
		{30, 7, true, "a longer finite window wins"},
		{7, 30, false, "a shorter finite window loses"},
		{7, 7, false, "equal windows are not wider"},
		{0, 30, true, "unlimited beats a finite window"},
		{0, 1, true, "unlimited beats the shortest finite window"},
		{30, 0, false, "a finite window loses to unlimited"},
		{0, 0, false, "two unlimited windows are equal, not wider"},
	}

	for _, tc := range tests {
		if got := RetentionWider(tc.a, tc.b); got != tc.want {
			t.Errorf("RetentionWider(%d, %d) = %v, want %v (%s)", tc.a, tc.b, got, tc.want, tc.why)
		}
	}
}

// TestWiderThanWouldHaveBeenWrong pins the actual defect: `>` reads 0 as the
// smallest possible window, so every "longest promise wins" fold silently
// shortened an unlimited project to the largest finite value beside it.
func TestWiderThanWouldHaveBeenWrong(t *testing.T) {
	if RetentionWider(0, 30) == (0 > 30) {
		t.Fatal("RetentionWider agrees with > for the unlimited case; the helper is pointless")
	}
	if 0 > 30 {
		t.Fatal("0 > 30; this test's premise is wrong")
	}
}

func TestLongestRetentionWindow(t *testing.T) {
	tests := []struct {
		name        string
		defaultDays int
		keyDays     []int
		want        int
	}{
		{
			name:        "no keys falls back to the configured default",
			defaultDays: 30,
			keyDays:     nil,
			want:        30,
		},
		{
			name:        "a deferring key inherits the default",
			defaultDays: 30,
			keyDays:     []int{0},
			want:        30,
		},
		{
			name:        "the longest explicit window wins",
			defaultDays: 30,
			keyDays:     []int{7, 90, 14},
			want:        90,
		},
		{
			name:        "a deferring key still honours a longer explicit sibling",
			defaultDays: 30,
			keyDays:     []int{0, 90},
			want:        90,
		},

		// The regression. This is the shape of the deployment that reported
		// "it says unlimited but I only see 7 days": APP_RETENTION_DAYS=0, the
		// project's own key at 0, and a sibling key still carrying an explicit
		// 7 from when the default was 7.
		{
			name:        "unlimited default with an unlimited key is unlimited",
			defaultDays: 0,
			keyDays:     []int{0},
			want:        0,
		},
		{
			name:        "unlimited default beside an explicit 7 is unlimited",
			defaultDays: 0,
			keyDays:     []int{7, 0},
			want:        0,
		},
		{
			name:        "unlimited default, explicit siblings only, keeps the longest",
			defaultDays: 0,
			keyDays:     []int{7, 30},
			want:        30,
		},
		{
			name:        "an unlimited key outranks a finite default",
			defaultDays: 30,
			keyDays:     []int{0, 90, 0},
			want:        90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LongestRetentionWindow(tc.defaultDays, tc.keyDays); got != tc.want {
				t.Errorf("LongestRetentionWindow(%d, %v) = %d, want %d",
					tc.defaultDays, tc.keyDays, got, tc.want)
			}
		})
	}
}

// TestResolveRetentionWindows covers the fold the object-store resolver and the
// dashboard both depend on: per-namespace windows, plus the widest of them all.
func TestResolveRetentionWindows(t *testing.T) {
	rows := []keyRetentionRow{
		{UserID: 7, Name: "unlimited-project", RetentionDays: 0},
		{UserID: 7, Name: "week-project", RetentionDays: 7},
		{UserID: 7, Name: "week-project-staging", RetentionDays: 0},
		{UserID: 9, Name: "unlimited-project", RetentionDays: 0},
	}

	byProject, widest := resolveRetentionWindows(rows, 0)

	// An unlimited key keeps its project unlimited rather than being read as
	// "narrower than 7".
	if got := byProject["7|unlimited-project"]; got != 0 {
		t.Errorf("unlimited project resolved to %d days, want 0 (unlimited)", got)
	}
	if got := byProject["7|week-project"]; got != 7 {
		t.Errorf("explicit 7-day project resolved to %d days, want 7", got)
	}
	// A deferring key under an unlimited default inherits unlimited.
	if got := byProject["7|week-project-staging"]; got != 0 {
		t.Errorf("deferring key under an unlimited default resolved to %d days, want 0", got)
	}
	// Same name under a different user is a different namespace, with its own
	// window.
	if got := byProject["9|unlimited-project"]; got != 0 {
		t.Errorf("second user's namespace resolved to %d days, want 0", got)
	}

	// The widest window in play decides when a whole hour bucket may be
	// deleted. One unlimited project means never.
	if widest != 0 {
		t.Errorf("widest = %d, want 0 (nothing may be deleted while a project keeps everything)", widest)
	}

	// And with no unlimited project, the widest is the longest finite window.
	finite, widestFinite := resolveRetentionWindows([]keyRetentionRow{
		{UserID: 7, Name: "a", RetentionDays: 7},
		{UserID: 7, Name: "b", RetentionDays: 90},
	}, 30)
	if widestFinite != 90 {
		t.Errorf("widest = %d, want 90", widestFinite)
	}
	if finite["7|a"] != 7 || finite["7|b"] != 90 {
		t.Errorf("finite windows resolved wrong: %v", finite)
	}
}

// TestResolveRetentionWindowsUnlimitedDefaultIsSafeWithoutKeys: an empty key
// table under an unlimited default must not look like a finite ceiling, or the
// retention pass would start deleting on a fresh install.
func TestResolveRetentionWindowsUnlimitedDefaultIsSafeWithoutKeys(t *testing.T) {
	_, widest := resolveRetentionWindows(nil, 0)
	if widest != 0 {
		t.Errorf("widest = %d with no keys and an unlimited default, want 0", widest)
	}

	_, widest = resolveRetentionWindows(nil, 30)
	if widest != 30 {
		t.Errorf("widest = %d with no keys and a 30-day default, want 30", widest)
	}
}
