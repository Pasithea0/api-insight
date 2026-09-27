package db

import (
	"context"
	"errors"
	"testing"
)

// fakeReader is a scripted EventReader: it records the queries it received and
// returns canned results or a canned error.
type fakeReader struct {
	name    string
	events  []Event
	byID    *Event
	err     error
	idErr   error
	got     []EventQuery
	gotIDs  []string
	byIDNil bool
}

func (f *fakeReader) SourceName() string { return f.name }

func (f *fakeReader) QueryEvents(_ context.Context, q EventQuery) ([]Event, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

func (f *fakeReader) EventByID(_ context.Context, id string) (*Event, error) {
	f.gotIDs = append(f.gotIDs, id)
	if f.idErr != nil {
		return nil, f.idErr
	}
	if f.byIDNil {
		return nil, nil
	}
	return f.byID, nil
}

func TestRawEventsResolve(t *testing.T) {
	pg := &fakeReader{name: "postgres"}
	ch := &fakeReader{name: "clickhouse"}

	tests := []struct {
		name        string
		defaultSrc  string
		override    string
		wantSource  string
		wantDefault string
		nilClickHse bool
	}{
		{name: "default postgres", defaultSrc: "postgres", wantSource: "postgres", wantDefault: "postgres"},
		{name: "default clickhouse", defaultSrc: "clickhouse", wantSource: "clickhouse", wantDefault: "clickhouse"},
		{name: "override to clickhouse", defaultSrc: "postgres", override: "clickhouse", wantSource: "clickhouse", wantDefault: "postgres"},
		{name: "override to postgres", defaultSrc: "clickhouse", override: "postgres", wantSource: "postgres", wantDefault: "clickhouse"},
		{name: "unknown override falls back to default", defaultSrc: "clickhouse", override: "banana", wantSource: "clickhouse", wantDefault: "clickhouse"},
		{name: "case and space insensitive", defaultSrc: "postgres", override: " ClickHouse ", wantSource: "clickhouse", wantDefault: "postgres"},
		{name: "no clickhouse ignores override asking for it", defaultSrc: "postgres", override: "clickhouse", wantSource: "postgres", wantDefault: "postgres", nilClickHse: true},
		{name: "no clickhouse refuses a clickhouse default", defaultSrc: "clickhouse", wantSource: "postgres", wantDefault: "postgres", nilClickHse: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var chReader EventReader = ch
			if tc.nilClickHse {
				chReader = nil
			}
			r := NewRawEvents(pg, chReader, tc.defaultSrc)
			if got := r.Resolve(tc.override).SourceName(); got != tc.wantSource {
				t.Errorf("Resolve(%q) = %q, want %q", tc.override, got, tc.wantSource)
			}
			// DefaultSource reports the configured default, independent of
			// any per-request override.
			if got := r.DefaultSource(); got != tc.wantDefault {
				t.Errorf("DefaultSource() = %q, want %q", got, tc.wantDefault)
			}
		})
	}
}

// TestRawEventsFallsBackOnError is the resilience property: a broken mirror
// must degrade to the system of record, not blank the dashboard.
func TestRawEventsFallsBackOnError(t *testing.T) {
	pg := &fakeReader{name: "postgres", events: []Event{{ID: 1}}}
	chDown := &fakeReader{name: "clickhouse", err: errors.New("connection refused")}

	r := NewRawEvents(pg, chDown, "clickhouse")
	events, source, err := r.QueryEvents(context.Background(), "", EventQuery{})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if source != "postgres" {
		t.Errorf("source = %q, want postgres (fallback)", source)
	}
	if len(events) != 1 {
		t.Errorf("got %d events, want 1", len(events))
	}
	if len(chDown.got) != 1 || len(pg.got) != 1 {
		t.Errorf("expected both stores to be tried: ch=%d pg=%d", len(chDown.got), len(pg.got))
	}
}

// TestRawEventsEmptyResultIsNotAFailure is the complement: zero rows is a
// legitimate answer and must be reported as coming from the chosen store,
// not silently retried against the other one.
func TestRawEventsEmptyResultIsNotAFailure(t *testing.T) {
	pg := &fakeReader{name: "postgres", events: []Event{{ID: 99}}}
	chEmpty := &fakeReader{name: "clickhouse", events: []Event{}}

	r := NewRawEvents(pg, chEmpty, "clickhouse")
	events, source, err := r.QueryEvents(context.Background(), "", EventQuery{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "clickhouse" {
		t.Errorf("source = %q, want clickhouse", source)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
	if len(pg.got) != 0 {
		t.Errorf("postgres should not have been consulted, got %d queries", len(pg.got))
	}
}

// TestRawEventsEventByIDFallsThrough covers the real topology: an event
// ingested before the mirror existed lives only in Postgres, while one that
// has aged out of Postgres lives only in the mirror. A miss in the selected
// store must therefore check the other before reporting not-found.
func TestRawEventsEventByIDFallsThrough(t *testing.T) {
	t.Run("found in mirror", func(t *testing.T) {
		pg := &fakeReader{name: "postgres", byIDNil: true}
		ch := &fakeReader{name: "clickhouse", byID: &Event{ID: 5}}
		r := NewRawEvents(pg, ch, "clickhouse")

		ev, err := r.EventByID(context.Background(), "", "5")
		if err != nil || ev == nil || ev.ID != 5 {
			t.Fatalf("got (%v, %v), want event 5", ev, err)
		}
		if len(pg.gotIDs) != 0 {
			t.Errorf("postgres should not have been consulted when the mirror had the row")
		}
	})

	t.Run("miss in mirror falls through to postgres", func(t *testing.T) {
		pg := &fakeReader{name: "postgres", byID: &Event{ID: 5, Project: "p"}}
		chMiss := &fakeReader{name: "clickhouse", byIDNil: true}
		r := NewRawEvents(pg, chMiss, "clickhouse")

		ev, err := r.EventByID(context.Background(), "", "5")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev == nil || ev.Project != "p" {
			t.Fatalf("got %+v, want the row from postgres", ev)
		}
		if len(chMiss.gotIDs) != 1 || len(pg.gotIDs) != 1 {
			t.Errorf("expected both stores to be tried: ch=%v pg=%v", chMiss.gotIDs, pg.gotIDs)
		}
	})

	t.Run("absent from both reports not found", func(t *testing.T) {
		pg := &fakeReader{name: "postgres", byIDNil: true}
		chMiss := &fakeReader{name: "clickhouse", byIDNil: true}
		r := NewRawEvents(pg, chMiss, "clickhouse")

		ev, err := r.EventByID(context.Background(), "", "404")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev != nil {
			t.Fatalf("got %+v, want nil", ev)
		}
	})

	t.Run("mirror error falls back to postgres", func(t *testing.T) {
		pg := &fakeReader{name: "postgres", byID: &Event{ID: 7}}
		chDown := &fakeReader{name: "clickhouse", idErr: errors.New("timeout")}
		r := NewRawEvents(pg, chDown, "clickhouse")

		ev, err := r.EventByID(context.Background(), "", "7")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev == nil || ev.ID != 7 {
			t.Fatalf("got %+v, want the postgres row", ev)
		}
	})
}

// TestPostgresFieldExpr checks the Postgres field mapping and its injection
// boundary, mirroring the ClickHouse side.
func TestPostgresFieldExpr(t *testing.T) {
	ok := map[string]string{
		"route":     "route",
		"path":      "route",
		"method":    "method",
		"remote_ip": "remote_ip",
		"status":    "CAST(status AS TEXT)",
		"region":    "attributes::jsonb ->> 'region'",
	}
	for field, want := range ok {
		got, isValid := PostgresFieldExpr(field)
		if !isValid || got != want {
			t.Errorf("PostgresFieldExpr(%q) = (%q, %v), want (%q, true)", field, got, isValid, want)
		}
	}
	for _, field := range []string{"", "ro ute", "ro'ute", "ro;ute", `ro"ute`, "ro-ute"} {
		if got, isValid := PostgresFieldExpr(field); isValid {
			t.Errorf("PostgresFieldExpr(%q) = (%q, true), want rejected", field, got)
		}
	}
}
