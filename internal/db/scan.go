package db

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/datatypes"
)

// ScanFilter is the store-independent predicate for a raw-event scan.
//
// It is the shape every non-SQL backing store evaluates in Go, so that a
// dashboard request means the same thing whichever store answers it. The
// fields deliberately mirror EventQuery: EventQuery is what the HTTP layer
// speaks, this is what a store can execute without SQL.
type ScanFilter struct {
	// UserID scopes to one tenant. Empty means "all tenants" (admin only).
	UserID string

	// Project narrows to one API key / project name.
	Project string

	// Status is "", "success" (<400) or "error" (>=400).
	Status string

	// AttrKey/AttrValue match one attribute pair. Both must be set.
	//
	// The comparison is string-typed on purpose, mirroring the Postgres
	// path: it filters with `attributes @> json_build_object(key, value)`,
	// and jsonb containment is type-sensitive, so a filter value of "5"
	// does NOT match a stored numeric 5. Being faithful here matters more
	// than being permissive — a store that matched more than Postgres would
	// make `?store=` comparisons meaningless.
	AttrKey   string
	AttrValue string

	// AttrExists keeps only events that carry this attribute key, whatever
	// its value. It mirrors Postgres `jsonb_exists(attributes, key)`.
	AttrExists string

	// Field names what Pattern is matched against: "route", "path",
	// "method", "remote_ip", "status" or an attribute key. Pattern is an
	// already-expanded LIKE pattern (see likeMatcher).
	Field   string
	Pattern string
}

// FromEventQuery derives a scan filter from the dashboard's EventQuery.
// AttrExists is left empty: EventQuery has no such concept, it is only used
// by the attribute/pattern panels.
func FromEventQuery(q EventQuery) ScanFilter {
	return ScanFilter{
		UserID:    q.UserID,
		Project:   q.Project,
		Status:    q.Status,
		AttrKey:   q.AttrKey,
		AttrValue: q.AttrValue,
		Field:     q.Field,
		Pattern:   q.Pattern,
	}
}

// IsEmpty reports whether the filter would accept every event.
func (f ScanFilter) IsEmpty() bool {
	return f.UserID == "" && f.Project == "" && f.Status == "" &&
		f.AttrKey == "" && f.AttrExists == "" && f.Field == ""
}

// Matcher is a ScanFilter with its LIKE pattern compiled once, so a scan
// does not recompile a regexp per event.
type Matcher struct {
	filter ScanFilter
	like   *likeRegexp
}

// NewMatcher compiles a filter. It fails only when the filter carries a LIKE
// pattern that cannot be rendered, which the Postgres path would also reject.
func NewMatcher(f ScanFilter) (Matcher, error) {
	m := Matcher{filter: f}
	if f.Field != "" && f.Pattern != "" {
		re, err := compileLike(f.Pattern)
		if err != nil {
			return Matcher{}, fmt.Errorf("invalid LIKE pattern %q: %w", f.Pattern, err)
		}
		m.like = re
	}
	return m, nil
}

// Filter returns the filter the matcher was built from.
func (m Matcher) Filter() ScanFilter { return m.filter }

// Match reports whether one event satisfies the filter.
func (m Matcher) Match(e Event) bool {
	f := m.filter
	if f.UserID != "" && e.UserID != f.UserID {
		return false
	}
	if f.Project != "" && e.Project != f.Project {
		return false
	}
	switch f.Status {
	case "success":
		if e.Status >= 400 {
			return false
		}
	case "error":
		if e.Status < 400 {
			return false
		}
	}
	if f.AttrKey != "" && f.AttrValue != "" {
		v, ok := attrStringValue(e.Attributes, f.AttrKey)
		if !ok || v != f.AttrValue {
			return false
		}
	}
	if f.AttrExists != "" {
		if _, ok := e.Attributes[f.AttrExists]; !ok {
			return false
		}
	}
	if m.like != nil {
		subject, ok := fieldValue(e, f.Field)
		if !ok {
			// Postgres evaluates LIKE against NULL for a missing attribute
			// (or an unknown column mapping), and NULL LIKE anything is
			// NULL — not a match.
			return false
		}
		if !m.like.match(subject) {
			return false
		}
	}
	return true
}

// attrStringValue returns an attribute's value, but only when it is a JSON
// string. Non-strings report ok=false so they are treated the same way
// Postgres jsonb containment treats them: not equal.
func attrStringValue(attrs datatypes.JSONMap, key string) (string, bool) {
	if attrs == nil {
		return "", false
	}
	v, ok := attrs[key]
	if !ok {
		return "", false
	}
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// fieldValue resolves a scan field to the string LIKE is applied to. ok is
// false for a field with no value on this event, mirroring SQL NULL.
func fieldValue(e Event, field string) (string, bool) {
	switch field {
	case "route", "path":
		// Routes are stored with their query string, so path and route are
		// the same value here, exactly as in the SQL stores.
		return e.Route, true
	case "method":
		return e.Method, true
	case "remote_ip":
		return e.RemoteIP, true
	case "status":
		return fmt.Sprint(e.Status), true
	default:
		if !SafeAttrKey.MatchString(field) {
			return "", false
		}
		return attrStringValue(e.Attributes, field)
	}
}

// EventScanner streams events in a time range from a backing store that
// cannot answer with SQL.
//
// It exists because the aggregate panels (traffic, latency, top routes,
// attribute breakdowns, pattern counts) and the rollup worker all read raw
// events, and none of that can be expressed against an object store. The
// contract is deliberately push-based — fn is called per event and the
// caller accumulates — so a store never has to materialise a whole window.
//
// Implementations must respect ctx and must not require fn to be
// concurrency-safe: fn is only ever called from one goroutine at a time,
// even if the store fetches objects concurrently.
type EventScanner interface {
	// ScanEvents calls fn for every event with Start <= created_at <= End
	// that matches filter. End is inclusive so that a request for "now"
	// does not race the newest events.
	ScanEvents(ctx context.Context, start, end time.Time, filter ScanFilter, fn func(Event) error) error

	// SourceName identifies the store, matching EventReader.
	SourceName() string
}

// likeRegexp is a compiled Postgres LIKE pattern.
type likeRegexp struct {
	re *regexp.Regexp
}

func (l *likeRegexp) match(s string) bool { return l.re.MatchString(s) }

// compileLike translates a SQL LIKE pattern into an equivalent regexp.
//
// Postgres LIKE semantics: `%` matches any sequence (including empty), `_`
// matches exactly one character, `\` escapes the next character, and the
// match is case-sensitive and anchored at both ends. Everything else is
// literal. `(?s)` makes `.` cross newlines so `%` still behaves as "any
// sequence" for multi-line subjects such as an attribute value with a
// newline in it.
func compileLike(pattern string) (*likeRegexp, error) {
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '%':
			b.WriteString(`.*`)
		case '_':
			b.WriteString(`.`)
		case '\\':
			// Escape the next character. A trailing backslash is not
			// valid in Postgres either; treat it as a literal so a bad
			// pattern degrades to "matches nothing useful" rather than
			// failing the request.
			if i+1 >= len(runes) {
				b.WriteString(regexp.QuoteMeta(string(r)))
				continue
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	return &likeRegexp{re: re}, nil
}
