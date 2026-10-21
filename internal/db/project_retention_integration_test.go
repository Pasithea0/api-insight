package db

import (
	"testing"
	"time"
)

// TestProjectRetentionEditReconcilesStoredEvents covers the write side of
// editing a project's retention.
//
// Ingest stamps expires_at at write time, so a retention edit has to reconcile
// the rows that already exist, not just the API key. The properties asserted
// here are the ones that make the edit honest:
//
//   - rows already older than the new window are deleted now;
//   - surviving rows carry the new expiry, so a shortened window is not left
//     honouring the old longer promise (the bug this exists to prevent);
//   - nothing outside the (user, project) pair changes: not another project of
//     the same user, not the same project name under another user;
//   - the pass is idempotent, so a repeated edit does nothing.
//
// Requires a throwaway server, like the rest of this suite:
//
//	APIINSIGHT_TEST_DATABASE_URL='postgres://user:***@localhost:55434/apiinsight_test?sslmode=disable' \
//	  go test ./internal/db/ -run ProjectRetention -v -count=1
func TestProjectRetentionEditReconcilesStoredEvents(t *testing.T) {
	gdb, err := Connect(testConfig(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	const (
		project    = "retention-edit-test"
		peerProj   = "retention-edit-peer-test"
		owner      = "retention-edit-user"
		otherOwner = "retention-edit-other-user"
	)

	cleanupProject(t, gdb, project, nil)
	cleanupProject(t, gdb, peerProj, nil)

	// Everything is truncated to the millisecond: Postgres keeps microseconds,
	// and the "already correct expiry is left alone" assertion below compares
	// expires_at to created_at + interval for equality. A nanosecond-precision
	// Go time would round on insert and make that comparison fail for a reason
	// that has nothing to do with retention.
	now := time.Now().UTC().Truncate(time.Millisecond)

	at := func(d time.Duration) time.Time { return now.Add(d) }
	expiry := func(created time.Time, days int) *time.Time {
		e := created.Add(time.Duration(days) * 24 * time.Hour)
		return &e
	}

	seed := func(user, proj, route string, created time.Time, expires *time.Time) Event {
		return Event{
			CreatedAt:  created,
			ExpiresAt:  expires,
			UserID:     user,
			Project:    proj,
			Route:      route,
			Method:     "GET",
			Status:     200,
			DurationMs: 1,
		}
	}

	var rows []Event
	for i := 0; i < 5; i++ {
		// Written under a 30 day window, already outside a 7 day one.
		old := at(-30 * 24 * time.Hour)
		rows = append(rows, seed(owner, project, "/older-than-window", old, expiry(old, 30)))

		// Inside a 7 day window, but still holding a 30 day promise: these are
		// the rows a naive edit leaves behind with 27 days of life left.
		long := at(-3 * 24 * time.Hour)
		rows = append(rows, seed(owner, project, "/long-promise", long, expiry(long, 30)))

		// No expiry at all ("never").
		rows = append(rows, seed(owner, project, "/no-expiry", at(-3*24*time.Hour), nil))

		// Already on the new window: must be left alone, not counted as a
		// rewrite.
		ok3d := at(-3 * 24 * time.Hour)
		rows = append(rows, seed(owner, project, "/already-correct", ok3d, expiry(ok3d, 7)))

		// Control groups: a different project of the same user, and the same
		// project name under a different user. Both must survive untouched.
		peer := at(-30 * 24 * time.Hour)
		rows = append(rows, seed(owner, peerProj, "/peer", peer, expiry(peer, 30)))
		rows = append(rows, seed(otherOwner, project, "/other-user", peer, expiry(peer, 30)))
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	count := func(where string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := gdb.Model(&Event{}).Where(where, args...).Count(&n).Error; err != nil {
			t.Fatalf("count (%s): %v", where, err)
		}
		return n
	}
	routeCount := func(user, proj, route string) int64 {
		return count("user_id = ? AND project = ? AND route = ?", user, proj, route)
	}
	// mismatched reports how many of a project's rows still disagree with the
	// window, whichever way they disagree.
	mismatched := func(user, proj string, days int) int64 {
		return count("user_id = ? AND project = ? AND expires_at IS DISTINCT FROM created_at + make_interval(days => ?)",
			user, proj, days)
	}

	// --- shrink 30 -> 7 days -------------------------------------------------
	res, err := ApplyProjectRetention(gdb, owner, project, 7)
	if err != nil {
		t.Fatalf("ApplyProjectRetention: %v", err)
	}
	if res.Deleted != 5 {
		t.Errorf("deleted = %d, want 5 (the rows older than 7 days)", res.Deleted)
	}
	if res.Rewritten != 10 {
		t.Errorf("rewritten = %d, want 10 (the long promise and no-expiry rows, not the 5 already on the window)", res.Rewritten)
	}

	if got := routeCount(owner, project, "/older-than-window"); got != 0 {
		t.Errorf("rows older than the new window remaining = %d, want 0", got)
	}
	for _, route := range []string{"/long-promise", "/no-expiry", "/already-correct"} {
		if got := routeCount(owner, project, route); got != 5 {
			t.Errorf("%s rows = %d, want 5 (a shrink must not delete rows inside the window)", route, got)
		}
	}
	if got := mismatched(owner, project, 7); got != 0 {
		t.Errorf("%d surviving rows still disagree with the 7 day window (the old longer expiry was left in place)", got)
	}
	if got := count("user_id = ? AND project = ? AND expires_at IS NULL", owner, project); got != 0 {
		t.Errorf("%d rows of a project with a window have no expiry, want 0", got)
	}

	// Control groups: untouched in both count and expiry.
	if got := count("user_id = ? AND project = ?", owner, peerProj); got != 5 {
		t.Errorf("peer project rows = %d, want 5", got)
	}
	if got := mismatched(owner, peerProj, 30); got != 0 {
		t.Error("peer project expiries were changed by another project's edit")
	}
	if got := count("user_id = ? AND project = ?", otherOwner, project); got != 5 {
		t.Errorf("other user's rows in the same project name = %d, want 5", got)
	}
	if got := mismatched(otherOwner, project, 30); got != 0 {
		t.Error("another user's expiries were changed by this user's edit")
	}

	// --- idempotent ----------------------------------------------------------
	again, err := ApplyProjectRetention(gdb, owner, project, 7)
	if err != nil {
		t.Fatalf("ApplyProjectRetention (repeat): %v", err)
	}
	if again.Deleted != 0 || again.Rewritten != 0 {
		t.Errorf("a repeated edit changed data: deleted=%d rewritten=%d, want 0/0", again.Deleted, again.Rewritten)
	}

	// --- grow 7 -> 45 days --------------------------------------------------
	// Growing deletes nothing: the rows are already inside the new window.
	// Their expiries must move out, though, or they would still expire on the
	// old schedule.
	grown, err := ApplyProjectRetention(gdb, owner, project, 45)
	if err != nil {
		t.Fatalf("ApplyProjectRetention (grow): %v", err)
	}
	if grown.Deleted != 0 {
		t.Errorf("growing the window deleted %d rows, want 0", grown.Deleted)
	}
	if got := mismatched(owner, project, 45); got != 0 {
		t.Errorf("%d rows did not move onto the 45 day window", got)
	}
	if got := count("user_id = ? AND project = ?", owner, project); got != 15 {
		t.Errorf("rows after growing = %d, want 15", got)
	}

	// --- argument guards -----------------------------------------------------
	if _, err := ApplyProjectRetention(gdb, owner, project, 0); err == nil {
		t.Error("a 0 day window must be rejected: 0 is the 'use the default' sentinel, not a policy")
	}
	if _, err := ApplyProjectRetention(gdb, "", project, 7); err == nil {
		t.Error("an empty userID must be rejected")
	}

	cleanupProject(t, gdb, project, nil)
	cleanupProject(t, gdb, peerProj, nil)
}

// TestEffectiveProjectRetentionUsesLongestSiblingKey pins the rule that keeps a
// retention edit from deleting a sibling key's data: keys that share a project
// name share one event namespace (events are stamped with the key NAME), so the
// window enforced has to be the longest one any of them asks for.
func TestEffectiveProjectRetentionUsesLongestSiblingKey(t *testing.T) {
	gdb, err := Connect(testConfig(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	const (
		username = "retention-edit-owner"
		project  = "retention-sibling-test"
		fallback = 30
	)

	// Re-runnable: clear any leftovers from an earlier failed run.
	if err := gdb.Where("username = ?", username).Delete(&User{}).Error; err != nil {
		t.Fatalf("cleanup user: %v", err)
	}
	user := User{Username: username, PasswordHash: "x"}
	if err := gdb.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		if err := gdb.Where("user_id = ?", user.ID).Delete(&APIKey{}).Error; err != nil {
			t.Errorf("cleanup keys: %v", err)
		}
		if err := gdb.Delete(&user).Error; err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	keys := []APIKey{
		{UserID: user.ID, Name: project, Environment: "prod", Key: project + "-prod", RetentionDays: 7},
		{UserID: user.ID, Name: project, Environment: "staging", Key: project + "-staging", RetentionDays: 0},
		{UserID: user.ID, Name: project, Environment: "dev", Key: project + "-dev", RetentionDays: 45},
		// A different project of the same user must not influence the result.
		{UserID: user.ID, Name: "unrelated-project", Environment: "prod", Key: project + "-other", RetentionDays: 365},
	}
	if err := gdb.Create(&keys).Error; err != nil {
		t.Fatalf("create keys: %v", err)
	}

	// 45 wins: it is the longest window any key of this project asks for.
	got, err := EffectiveProjectRetention(gdb, user.ID, project, fallback)
	if err != nil {
		t.Fatalf("EffectiveProjectRetention: %v", err)
	}
	if got != 45 {
		t.Errorf("window = %d, want 45 (the longest sibling key)", got)
	}

	// With the long key dropped, the remaining explicit key (7) loses to the
	// configured default (30), which the staging key defers to.
	if err := gdb.Where("key = ?", project+"-dev").Delete(&APIKey{}).Error; err != nil {
		t.Fatalf("delete long key: %v", err)
	}
	got, err = EffectiveProjectRetention(gdb, user.ID, project, fallback)
	if err != nil {
		t.Fatalf("EffectiveProjectRetention: %v", err)
	}
	if got != fallback {
		t.Errorf("window = %d, want %d (the configured default a sibling key defers to)", got, fallback)
	}
}
