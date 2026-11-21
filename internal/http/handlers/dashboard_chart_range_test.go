package handlers

import "testing"

// TestChartMaxDaysNeverReturnsZero is the invariant the caller leans on: it
// uses 0 to mean "not decided yet", and 0 is also the unlimited sentinel, so a
// ceiling function that could return 0 would reintroduce exactly the ambiguity
// this whole change exists to remove.
func TestChartMaxDaysNeverReturnsZero(t *testing.T) {
	for _, window := range []int{-5, -1, 0, 1, 7, 30, 90, 365, 3650} {
		got := chartMaxDays(window)
		if got == 0 {
			t.Errorf("chartMaxDays(%d) = 0, which collides with the 'not decided' sentinel", window)
		}
		if got < 0 {
			t.Errorf("chartMaxDays(%d) = %d, want a positive number of days", window, got)
		}
	}
}

// TestChartMaxDaysUnlimitedIsTheSelectorCeiling: unlimited data means the range
// dropdown offers its own ceiling. That is a UI limit, not a retention policy —
// the API accepts any range.
func TestChartMaxDaysUnlimitedIsTheSelectorCeiling(t *testing.T) {
	if got := chartMaxDays(0); got != unlimitedChartMaxDays {
		t.Errorf("chartMaxDays(0) = %d, want %d", got, unlimitedChartMaxDays)
	}
	if got := chartMaxDays(7); got != 7 {
		t.Errorf("chartMaxDays(7) = %d, want 7", got)
	}
}

// TestEffectiveRetentionWindow: a key asking for nothing inherits the
// configured default, and the configured default may itself be unlimited. It
// must not be turned into a number on the way through.
func TestEffectiveRetentionWindow(t *testing.T) {
	tests := []struct {
		name        string
		keyDays     int
		defaultDays int
		want        int
	}{
		{"explicit key window wins", 7, 30, 7},
		{"explicit key window beats unlimited default", 7, 0, 7},
		{"deferring key inherits a finite default", 0, 30, 30},
		{"deferring key inherits an unlimited default", 0, 0, 0},
		{"negative is treated as deferring", -1, 30, 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveRetentionWindow(tc.keyDays, tc.defaultDays); got != tc.want {
				t.Errorf("effectiveRetentionWindow(%d, %d) = %d, want %d",
					tc.keyDays, tc.defaultDays, got, tc.want)
			}
		})
	}
}
