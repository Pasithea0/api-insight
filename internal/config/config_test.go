package config

import (
	"testing"
)

// TestRetentionDaysZeroMeansUnlimited is the behaviour that matters: 0 is a
// policy, not a missing value. Ignoring it and applying the 30-day default
// leaves the operator believing they have disabled expiry while the service
// quietly deletes their history.
func TestRetentionDaysZeroMeansUnlimited(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		set         bool
		wantDays    int
		wantUnlimit bool
	}{
		{name: "unset falls back to the 30-day default", set: false, wantDays: 30},
		{name: "zero is unlimited", env: "0", set: true, wantDays: 0, wantUnlimit: true},
		{name: "positive is honoured", env: "7", set: true, wantDays: 7},
		{name: "negative is rejected", env: "-5", set: true, wantDays: 30},
		{name: "garbage is rejected", env: "forever", set: true, wantDays: 30},
		{name: "empty string is the default", env: "", set: true, wantDays: 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("APP_RETENTION_DAYS", tc.env)
			}
			cfg := Load()
			if cfg.RetentionDays != tc.wantDays {
				t.Errorf("RetentionDays = %d, want %d", cfg.RetentionDays, tc.wantDays)
			}
			if cfg.RetentionUnlimited() != tc.wantUnlimit {
				t.Errorf("RetentionUnlimited() = %v, want %v", cfg.RetentionUnlimited(), tc.wantUnlimit)
			}
		})
	}
}

// TestEventStoreDefaultsToPostgres guards the no-surprises property: a
// deployment that sets nothing keeps its events exactly where they were.
func TestEventStoreDefaultsToPostgres(t *testing.T) {
	cfg := Load()
	if cfg.EventStore != EventStorePostgres {
		t.Errorf("EventStore = %q, want %q", cfg.EventStore, EventStorePostgres)
	}
	if cfg.S3Enabled() {
		t.Error("S3Enabled() is true with no S3 configuration")
	}
}

// TestS3IncompleteConfigFallsBackToPostgres is the safety interlock: asking
// for the object store without the settings needed to reach it would drop
// every ingested event once the process accepted it, so it is refused up
// front and the events keep going to Postgres.
func TestS3IncompleteConfigFallsBackToPostgres(t *testing.T) {
	t.Setenv("APP_EVENT_STORE", "s3")
	t.Setenv("APP_S3_BUCKET", "a-bucket")
	// Endpoint and credentials deliberately absent.

	cfg := Load()
	if cfg.EventStore != EventStorePostgres {
		t.Errorf("EventStore = %q, want a fallback to %q", cfg.EventStore, EventStorePostgres)
	}
	if cfg.S3Enabled() {
		t.Error("S3Enabled() is true for a half-configured object store")
	}
}

// TestS3CompleteConfigIsAccepted is the other half: with everything present
// the mode sticks, and the defaults are the ones the runbook documents.
func TestS3CompleteConfigIsAccepted(t *testing.T) {
	t.Setenv("APP_EVENT_STORE", "s3")
	t.Setenv("APP_S3_ENDPOINT", "https://s3.us-east-005.backblazeb2.com/")
	t.Setenv("APP_S3_REGION", "us-east-005")
	t.Setenv("APP_S3_BUCKET", "tidb-api-traffic")
	t.Setenv("APP_S3_ACCESS_KEY_ID", "key-id")
	t.Setenv("APP_S3_SECRET_ACCESS_KEY", "secret")

	cfg := Load()
	if !cfg.S3Enabled() {
		t.Fatal("S3Enabled() is false for a complete configuration")
	}
	if cfg.S3Endpoint != "https://s3.us-east-005.backblazeb2.com" {
		t.Errorf("S3Endpoint = %q, want the trailing slash trimmed", cfg.S3Endpoint)
	}
	if cfg.S3Prefix != "events" {
		t.Errorf("S3Prefix = %q, want the default", cfg.S3Prefix)
	}
	if !cfg.S3PathStyle {
		t.Error("S3PathStyle should default to true (B2 and MinIO need it)")
	}
	if cfg.EventStore != EventStoreS3 {
		t.Errorf("EventStore = %q, want %q", cfg.EventStore, EventStoreS3)
	}
}
