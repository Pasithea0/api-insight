package clickhouse

import (
	"strings"
	"testing"
	"time"

	dbpkg "apiinsight/internal/db"
)

// TestBuildQuery checks the generated SQL and bind arguments for each filter
// combination the dashboard can produce. buildQuery is pure, so this needs
// no server.
func TestBuildQuery(t *testing.T) {
	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name      string
		q         dbpkg.EventQuery
		wantWhere []string
		wantArgN  int // args before LIMIT/OFFSET
	}{
		{
			name:      "time window only",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Limit: 10},
			wantWhere: []string{"created_at >= ?"},
			wantArgN:  1,
		},
		{
			name: "full filter set",
			q: dbpkg.EventQuery{
				Cutoff: cutoff, UserID: "7", Project: "proj",
				Status: "error", AttrKey: "region", AttrValue: "eu-west",
				Field: "route", Pattern: "%tt123%", Limit: 5,
			},
			wantWhere: []string{
				"created_at >= ?", "user_id = ?", "project = ?",
				"status >= 400", "attributes['region'] = ?", "route LIKE ?",
			},
			// cutoff, user_id, project, attr value, pattern -- the status
			// clause is a literal comparison and binds nothing.
			wantArgN: 5,
		},
		{
			name:      "success filter is a strict less-than",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Status: "success"},
			wantWhere: []string{"created_at >= ?", "status < 400"},
			wantArgN:  1,
		},
		{
			name:      "unknown status is ignored rather than guessed",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Status: "banana"},
			wantWhere: []string{"created_at >= ?"},
			wantArgN:  1,
		},
		{
			name:      "attribute key maps to a map subscript",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Field: "region", Pattern: "%eu%"},
			wantWhere: []string{"created_at >= ?", "attributes['region'] LIKE ?"},
			wantArgN:  2,
		},
		{
			name:      "path is the same column as route",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Field: "path", Pattern: "/v3/%"},
			wantWhere: []string{"created_at >= ?", "route LIKE ?"},
			wantArgN:  2,
		},
		{
			name:      "status field search compares as text",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Field: "status", Pattern: "50%"},
			wantWhere: []string{"created_at >= ?", "toString(status) LIKE ?"},
			wantArgN:  2,
		},
		{
			name:      "field without pattern adds no clause",
			q:         dbpkg.EventQuery{Cutoff: cutoff, Field: "route"},
			wantWhere: []string{"created_at >= ?"},
			wantArgN:  1,
		},
		{
			name:      "attr key without value adds no clause",
			q:         dbpkg.EventQuery{Cutoff: cutoff, AttrKey: "region"},
			wantWhere: []string{"created_at >= ?"},
			wantArgN:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, _, err := buildQuery(tc.q)
			if err != nil {
				t.Fatalf("buildQuery: %v", err)
			}
			if !strings.Contains(sql, "FROM "+EventsTable) {
				t.Errorf("sql does not select from %s: %s", EventsTable, sql)
			}
			if !strings.Contains(sql, "ORDER BY created_at DESC") {
				t.Errorf("sql is not newest-first: %s", sql)
			}
			// The WHERE clause is everything between WHERE and ORDER BY.
			where := sql[strings.Index(sql, "WHERE ")+len("WHERE ") : strings.Index(sql, " ORDER BY")]
			got := strings.Split(where, " AND ")
			if len(got) != len(tc.wantWhere) {
				t.Fatalf("where clause = %v, want %v", got, tc.wantWhere)
			}
			for i := range got {
				if got[i] != tc.wantWhere[i] {
					t.Errorf("where[%d] = %q, want %q", i, got[i], tc.wantWhere[i])
				}
			}
			// args = filter args + limit + offset
			if len(args) != tc.wantArgN+2 {
				t.Errorf("arg count = %d, want %d (+limit/offset)", len(args), tc.wantArgN)
			}
		})
	}
}

// TestBuildQueryRejectsInjection is the guard for the one place this package
// interpolates instead of binding. An attribute key or field name that is not
// a bare identifier must be refused outright, never spliced into the SQL.
func TestBuildQueryRejectsInjection(t *testing.T) {
	cutoff := time.Now().UTC()

	malicious := []string{
		`region' = 'x' OR 1=1 --`,
		`region"] OR 1=1 --`,
		`region; DROP TABLE events`,
		`region) OR (1=1`,
		`region key`,
		`region-key`,
		`region.key`,
		`region'`,
		` région`,
	}

	for _, bad := range malicious {
		t.Run(bad, func(t *testing.T) {
			// As an attribute key.
			if sql, _, _, err := buildQuery(dbpkg.EventQuery{
				Cutoff: cutoff, AttrKey: bad, AttrValue: "v",
			}); err == nil {
				t.Errorf("attribute key %q was accepted; generated SQL: %s", bad, sql)
			}
			// As a search field.
			if sql, _, _, err := buildQuery(dbpkg.EventQuery{
				Cutoff: cutoff, Field: bad, Pattern: "%x%",
			}); err == nil {
				t.Errorf("search field %q was accepted; generated SQL: %s", bad, sql)
			}
		})
	}
}

// TestBuildQueryEmptyMeansAbsent documents that an empty key/field is treated
// as "no filter" rather than as a rejected identifier: the HTTP layer always
// passes through whatever query string it received, so empty must be a no-op
// and never a validation error.
func TestBuildQueryEmptyMeansAbsent(t *testing.T) {
	cutoff := time.Now().UTC()

	// buildQuery rejects a malformed NON-empty key, which is the boundary
	// TestBuildQueryRejectsInjection covers. Empty simply adds no clause.
	for _, q := range []dbpkg.EventQuery{
		{Cutoff: cutoff, AttrKey: "", AttrValue: "v"},
		{Cutoff: cutoff, Field: "", Pattern: "%x%"},
		{Cutoff: cutoff, AttrKey: "", AttrValue: ""},
	} {
		sql, _, _, err := buildQuery(q)
		if err != nil {
			t.Errorf("empty key/field should be treated as absent, got %v", err)
		}
		if strings.Contains(sql, "attributes[") || strings.Contains(sql, "LIKE") {
			t.Errorf("empty key/field produced a filter clause: %s", sql)
		}
	}
}

// TestBuildQueryLimits proves the limit is clamped and the offset cannot go
// negative, so a caller cannot ask for an unbounded scan.
func TestBuildQueryLimits(t *testing.T) {
	cutoff := time.Now().UTC()

	tests := []struct {
		name       string
		limit      int
		offset     int
		wantLimit  int
		wantOffset int
	}{
		{"default applied when unset", 0, 0, 100, 0},
		{"negative limit falls back to default", -5, 0, 100, 0},
		{"clamped to the maximum", 100000, 0, maxQueryLimit, 0},
		{"offset passes through", 10, 250, 10, 250},
		{"negative offset becomes zero", 10, -3, 10, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, args, limit, err := buildQuery(dbpkg.EventQuery{
				Cutoff: cutoff, Limit: tc.limit, Offset: tc.offset,
			})
			if err != nil {
				t.Fatalf("buildQuery: %v", err)
			}
			if limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", limit, tc.wantLimit)
			}
			// limit and offset are the final two bind arguments.
			gotOffset := args[len(args)-1]
			if gotOffset != tc.wantOffset {
				t.Errorf("offset = %v, want %d", gotOffset, tc.wantOffset)
			}
		})
	}
}

// TestChFieldExpr checks the field-name mapping, including the deliberate
// rejection of anything that is not a bare identifier.
func TestChFieldExpr(t *testing.T) {
	ok := map[string]string{
		"route":     "route",
		"path":      "route",
		"method":    "method",
		"remote_ip": "remote_ip",
		"status":    "toString(status)",
		"region":    "attributes['region']",
		"tier_2":    "attributes['tier_2']",
	}
	for field, want := range ok {
		got, isValid := chFieldExpr(field)
		if !isValid || got != want {
			t.Errorf("chFieldExpr(%q) = (%q, %v), want (%q, true)", field, got, isValid, want)
		}
	}

	for _, field := range []string{"", "ro ute", "ro'ute", "ro;ute", "ro-ute", "ro.ute"} {
		if got, isValid := chFieldExpr(field); isValid {
			t.Errorf("chFieldExpr(%q) = (%q, true), want rejected", field, got)
		}
	}
}

// TestStringMapConversion guards the ingest-side value flattening that the
// read path reverses: every value becomes a string, nil becomes empty.
func TestStringMapConversion(t *testing.T) {
	got := stringMap(map[string]any{
		"region": "eu-west",
		"tier":   2,
		"flag":   true,
		"nil":    nil,
	})
	want := map[string]string{
		"region": "eu-west",
		"tier":   "2",
		"flag":   "true",
		"nil":    "",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("stringMap[%q] = %q, want %q", k, got[k], v)
		}
	}
}
