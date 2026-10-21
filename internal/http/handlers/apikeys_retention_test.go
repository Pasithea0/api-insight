package handlers

import "testing"

// TestParseRetentionDays pins the validation rules of the retention field on
// the settings form. Getting these wrong is not cosmetic: the value ends up in
// an API key row and from there in the window that deletes events, so a silent
// acceptance of a typo, or of a value above what the service is allowed to
// honour, would turn a form field into a data-loss policy.
func TestParseRetentionDays(t *testing.T) {
	const max = 30

	cases := []struct {
		name string
		raw  string
		want int
		err  bool
	}{
		{name: "empty means the configured default", raw: "", want: max},
		{name: "whitespace is not a value", raw: "   ", want: max},
		{name: "zero means the configured default", raw: "0", want: max},
		{name: "a plain value is kept", raw: "7", want: 7},
		{name: "surrounding whitespace is tolerated", raw: " 14 ", want: 14},
		{name: "the maximum itself is valid", raw: "30", want: 30},
		{name: "above the maximum is clamped, not stored", raw: "365", want: max},
		{name: "negative is rejected", raw: "-1", err: true},
		{name: "garbage is rejected", raw: "abc", err: true},
		{name: "a float is rejected", raw: "7.5", err: true},
		{name: "a value with a unit is rejected", raw: "7d", err: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRetentionDays(tc.raw, max)
			if tc.err {
				if err == nil {
					t.Fatalf("parseRetentionDays(%q) = %d, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRetentionDays(%q) returned %v, want %d", tc.raw, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("parseRetentionDays(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseRetentionDaysZeroMax guards the misconfiguration where no maximum is
// configured: an explicit value must survive rather than be clamped to nothing.
func TestParseRetentionDaysZeroMax(t *testing.T) {
	got, err := parseRetentionDays("14", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 14 {
		t.Fatalf("got %d, want 14", got)
	}
}
