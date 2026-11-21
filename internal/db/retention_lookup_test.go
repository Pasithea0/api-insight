package db

import "testing"

// TestRetentionResolverUnlimited covers APP_RETENTION_DAYS=0 on the object-store
// read path: the resolver has to answer "no window at all" rather than falling
// back to a number, because the reader turns a zero window into "keep
// everything" and any other answer would silently expire history the operator
// asked to keep.
//
// The resolver is built over a nil handle on purpose. That is the shape the
// question has in these tests — the arithmetic and the defaults — and the
// refresh path is a documented no-op without a database, so the fallbacks are
// exactly what a real deployment with no api_keys rows would see.
func TestRetentionResolverUnlimited(t *testing.T) {
	unlimited := NewRetentionResolver(nil, 0)

	if got := unlimited.DefaultRetentionDays(); got != 0 {
		t.Errorf("DefaultRetentionDays() = %d, want 0", got)
	}
	if got := unlimited.ProjectRetentionDays("7", "payments"); got != 0 {
		t.Errorf("ProjectRetentionDays() = %d, want 0 (unlimited), not a fallback window", got)
	}
	if got := unlimited.MaxRetentionDays(); got != 0 {
		t.Errorf("MaxRetentionDays() = %d, want 0 so whole objects are never deleted", got)
	}
}

// TestRetentionResolverLimited is the contrast case: a configured window is
// reported as itself, so unlimited is a deliberate state rather than what an
// unconfigured resolver happens to return.
func TestRetentionResolverLimited(t *testing.T) {
	limited := NewRetentionResolver(nil, 30)

	if got := limited.DefaultRetentionDays(); got != 30 {
		t.Errorf("DefaultRetentionDays() = %d, want 30", got)
	}
	if got := limited.ProjectRetentionDays("7", "unknown-project"); got != 30 {
		t.Errorf("ProjectRetentionDays() = %d, want the default 30", got)
	}
	if got := limited.MaxRetentionDays(); got != 30 {
		t.Errorf("MaxRetentionDays() = %d, want 30", got)
	}
}

// TestRetentionResolverIsNilSafe: the resolver is optional wiring, and a
// caller that never set one must get a harmless answer rather than a panic on
// the read path of every dashboard request.
func TestRetentionResolverIsNilSafe(t *testing.T) {
	var nilResolver *RetentionResolver

	if got := nilResolver.DefaultRetentionDays(); got != 0 {
		t.Errorf("DefaultRetentionDays() = %d, want 0", got)
	}
	if got := nilResolver.ProjectRetentionDays("7", "p"); got != 0 {
		t.Errorf("ProjectRetentionDays() = %d, want 0", got)
	}
	if got := nilResolver.MaxRetentionDays(); got != 0 {
		t.Errorf("MaxRetentionDays() = %d, want 0", got)
	}
}
