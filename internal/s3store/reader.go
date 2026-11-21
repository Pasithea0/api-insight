package s3store

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	dbpkg "apiinsight/internal/db"
)

// Bounds on a single read.
const (
	// defaultQueryLimit mirrors the SQL readers' default page.
	defaultQueryLimit = 100

	// maxQueryLimit caps one read regardless of what the caller asked for,
	// so a hand-crafted ?limit= cannot turn into an unbounded download.
	maxQueryLimit = 5000

	// maxScanHours caps how many hour buckets one request may walk, so a
	// range of "everything" cannot become an unbounded listing.
	maxScanHours = 24 * 366

	// defaultReadConcurrency is how many objects of one hour are fetched at
	// once. Higher would be faster per hour and worse for everything else
	// sharing the connection.
	defaultReadConcurrency = 8
)

// errStopScan unwinds a scan once the caller has everything it needs. It is
// returned by the internal helpers and consumed by the public methods, so it
// never escapes the package.
var errStopScan = errors.New("s3store: scan complete")

// ErrObjectNotFound reports a key that is gone. Retention deletes objects
// while reads may be in flight, so this is an expected race and callers skip
// the object rather than failing the request.
var ErrObjectNotFound = errors.New("s3store: object not found")

// ReaderOptions configures a Reader.
type ReaderOptions struct {
	Prefix      string
	ReadTimeout time.Duration
	Concurrency int

	// Retention resolves how long a project's events live. When nil, no
	// event is filtered out on age and the store serves whatever it holds.
	Retention dbpkg.ProjectRetentionLookup
}

// Reader serves raw-event reads from the object store.
//
// It implements db.EventReader and db.EventScanner, so it can stand in for
// the Postgres reader everywhere the dashboard or the rollup worker asks for
// raw events.
type Reader struct {
	store ObjectStore
	opts  ReaderOptions
}

// NewReader builds a Reader over an already-opened store.
func NewReader(store ObjectStore, opts ReaderOptions) *Reader {
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultReadConcurrency
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 30 * time.Second
	}
	return &Reader{store: store, opts: opts}
}

// SourceName implements db.EventReader and db.EventScanner.
func (r *Reader) SourceName() string { return "s3" }

// QueryEvents implements db.EventReader: newest-first, filtered, paged.
//
// Hour buckets are walked newest-first and the walk stops as soon as enough
// rows have been collected, which keeps a sparse request to a couple of GETs.
//
// The stop happens on an *hour boundary*, never inside one. Object keys carry
// no reliable intra-hour time order (a backdated batch is written into an
// older bucket with a newer id), so abandoning an hour part-way through would
// return an arbitrary subset of it rather than its newest events — a page
// that looks plausible and is quietly wrong. The cost is that paging into a
// busy hour decodes that hour; memory stays bounded because only the newest
// `need` rows of an hour are ever retained.
func (r *Reader) QueryEvents(ctx context.Context, q dbpkg.EventQuery) ([]dbpkg.Event, error) {
	matcher, err := dbpkg.NewMatcher(dbpkg.FromEventQuery(q))
	if err != nil {
		return nil, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	need := limit + offset

	now := time.Now().UTC()
	start := r.effectiveStart(q.Cutoff, now)
	keep := newRetentionFilter(r.opts.Retention, now)

	var out []dbpkg.Event
	// One hour of headroom past "now" so an event with a timestamp slightly
	// ahead of the server clock is not silently unreachable: the writer
	// files events under their own hour, future ones included.
	hours := hoursBetween(start, now.Add(time.Hour), maxScanHours)

	for _, hour := range hours {
		hourEvents := make([]dbpkg.Event, 0, 256)

		err := r.streamHour(ctx, hour, matcher, keep, func(ev dbpkg.Event) error {
			hourEvents = append(hourEvents, ev)
			if len(hourEvents) >= 2*need {
				// Keep only the newest `need` seen so far: a busy hour must
				// not be able to grow this slice without bound.
				sortNewestFirst(hourEvents)
				hourEvents = hourEvents[:need]
			}
			return nil
		})
		if err != nil {
			return nil, err
		}

		sortNewestFirst(hourEvents)
		out = append(out, hourEvents...)
		if len(out) >= need {
			break
		}
	}

	// The concatenation is already ordered (newer buckets first, each sorted
	// internally). The sort is kept as a cheap guarantee, because a sequence
	// carry on a same-millisecond burst can push an id one millisecond into
	// the next hour bucket.
	sortNewestFirst(out)

	if offset >= len(out) {
		return []dbpkg.Event{}, nil
	}
	if offset+limit > len(out) {
		limit = len(out) - offset
	}
	return out[offset : offset+limit], nil
}

// sortNewestFirst orders events the way every raw-event response is ordered,
// tie-breaking on the id so the result is reproducible rather than dependent
// on which object happened to be read first.
func sortNewestFirst(events []dbpkg.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].CreatedAt.After(events[j].CreatedAt)
		}
		return events[i].ID > events[j].ID
	})
}

// EventByID implements db.EventReader.
//
// This is the one read the timestamp-in-the-id trick exists for: the hour
// bucket is derived from the id, so a detail lookup is one listing instead
// of a scan. The neighbouring hours are checked too, because a sequence
// carry on a same-millisecond burst can push an id into the next
// millisecond, and at exactly HH:00:00.000 that is the next hour.
//
// A miss returns (nil, nil) like the SQL readers, and an id that could not
// have come from this generator — an integer a previous Postgres deployment
// handed out — also returns (nil, nil), which is what lets the resolver fall
// through to Postgres for pre-migration rows.
func (r *Reader) EventByID(ctx context.Context, id string) (*dbpkg.Event, error) {
	parsed, err := strconv.ParseUint(id, 10, 64)
	if err != nil || parsed == 0 {
		return nil, nil
	}
	hour, ok := IDHour(uint(parsed))
	if !ok {
		return nil, nil
	}

	now := time.Now().UTC()
	keep := newRetentionFilter(r.opts.Retention, now)

	for _, h := range []time.Time{hour, hour.Add(-time.Hour), hour.Add(time.Hour)} {
		var found *dbpkg.Event
		err := r.streamHour(ctx, h, dbpkg.Matcher{}, keep, func(ev dbpkg.Event) error {
			if ev.ID != uint(parsed) {
				return nil
			}
			e := ev
			found = &e
			return errStopScan
		})
		if err != nil && !errors.Is(err, errStopScan) {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, nil
}

// ScanEvents implements db.EventScanner. Every event in [start, end] that
// matches filter is passed to fn, fn is called serially, and the walk stops
// on the first error fn returns.
func (r *Reader) ScanEvents(ctx context.Context, start, end time.Time, filter dbpkg.ScanFilter, fn func(dbpkg.Event) error) error {
	matcher, err := dbpkg.NewMatcher(filter)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	keep := newRetentionFilter(r.opts.Retention, now)

	for _, hour := range hoursBetween(start, end, maxScanHours) {
		if err := r.streamHour(ctx, hour, matcher, keep, fn); err != nil {
			return err
		}
	}
	return nil
}

// effectiveStart clamps a query's lower bound to the oldest data the store
// could still hold, so a request for "everything" does not walk hour buckets
// that were deleted months ago.
func (r *Reader) effectiveStart(cutoff, now time.Time) time.Time {
	start := cutoff.UTC()
	if cutoff.IsZero() {
		start = now.Add(-24 * time.Hour)
	}
	if r.opts.Retention != nil {
		if days := r.opts.Retention.MaxRetentionDays(); days > 0 {
			floor := now.Add(-time.Duration(days) * 24 * time.Hour)
			if start.Before(floor) {
				start = floor
			}
		}
	}
	return start
}

// streamHour fetches every object of one hour bucket, applies the filter and
// the age policy, and passes survivors to emit.
//
// Objects are fetched concurrently but emitted serially: a store must never
// hand a caller a callback it has to synchronise.
func (r *Reader) streamHour(
	ctx context.Context,
	hour time.Time,
	matcher dbpkg.Matcher,
	keep *retentionFilter,
	emit func(dbpkg.Event) error,
) error {
	prefix := hourPrefix(r.opts.Prefix, hour)
	if err := mustPrefix(prefix); err != nil {
		return err
	}

	var keys []string
	if err := r.store.List(ctx, prefix, func(key string) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}

	workers := r.opts.Concurrency
	for i := 0; i < len(keys); i += workers {
		end := i + workers
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		decoded := make([][]dbpkg.Event, len(batch))
		errs := make([]error, len(batch))

		var wg sync.WaitGroup
		for j, key := range batch {
			wg.Add(1)
			go func(j int, key string) {
				defer wg.Done()
				readCtx, cancel := context.WithTimeout(ctx, r.opts.ReadTimeout)
				defer cancel()
				data, err := r.store.Get(readCtx, key)
				if err != nil {
					errs[j] = err
					return
				}
				events, err := decodeEvents(data)
				if err != nil {
					errs[j] = err
					return
				}
				decoded[j] = events
			}(j, key)
		}
		wg.Wait()

		for j := range batch {
			switch {
			case errs[j] == nil:
			case errors.Is(errs[j], ErrObjectNotFound), errors.Is(errs[j], io.EOF):
				// Retention removed it between the listing and the read.
				continue
			default:
				return errs[j]
			}
			for _, ev := range decoded[j] {
				if !keep.accepts(ev) {
					continue
				}
				if !matcher.Match(ev) {
					continue
				}
				if err := emit(ev); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// retentionFilter drops events that have aged out of their project's window.
//
// This is how per-project retention is enforced on a store whose objects
// cannot be rewritten: rather than stamping an expiry into an object and
// trusting it forever, the current window is resolved and applied as the
// data is read, so shortening a window takes effect on the next request.
type retentionFilter struct {
	lookup dbpkg.ProjectRetentionLookup
	now    time.Time
	memo   map[string]time.Time
}

func newRetentionFilter(lookup dbpkg.ProjectRetentionLookup, now time.Time) *retentionFilter {
	return &retentionFilter{lookup: lookup, now: now, memo: map[string]time.Time{}}
}

func (f *retentionFilter) accepts(ev dbpkg.Event) bool {
	if f == nil || f.lookup == nil {
		return true
	}
	key := ev.UserID + "|" + ev.Project
	cutoff, ok := f.memo[key]
	if !ok {
		days := f.lookup.ProjectRetentionDays(ev.UserID, ev.Project)
		if days > 0 {
			cutoff = f.now.Add(-time.Duration(days) * 24 * time.Hour)
		}
		f.memo[key] = cutoff
	}
	if cutoff.IsZero() {
		return true
	}
	return !ev.CreatedAt.Before(cutoff)
}
