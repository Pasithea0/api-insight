package s3store

import (
	"context"
	"fmt"
	"time"

	dbpkg "apiinsight/internal/db"
)

// Options is everything a Store needs.
type Options struct {
	// Endpoint is the S3-compatible endpoint, with or without scheme.
	Endpoint string

	// Region is the signing region.
	Region string

	// Bucket holds the event objects.
	Bucket string

	// AccessKeyID and SecretAccessKey are the credentials. On Backblaze B2
	// these are the application key ID and the application key.
	AccessKeyID     string
	SecretAccessKey string

	// Prefix is the key prefix, e.g. "events".
	Prefix string

	// PathStyle puts the bucket in the path rather than the host. B2 and
	// MinIO want this.
	PathStyle bool

	// FlushInterval is how long events buffer before becoming an object.
	FlushInterval time.Duration

	// MaxBufferEvents caps the in-memory buffer.
	MaxBufferEvents int

	// ReadTimeout bounds one object read.
	ReadTimeout time.Duration

	// Concurrency is how many objects of one hour are decoded at once.
	Concurrency int

	// Retention resolves per-project windows for read-time filtering.
	Retention dbpkg.ProjectRetentionLookup
}

// Store is the object store as the rest of the service sees it: a sink for
// ingested events, a reader for the raw-event panels, a scanner for the
// rollup worker, and a retention target. One type, because all four operate
// on the same objects and splitting them would only add wiring.
type Store struct {
	client ObjectStore
	writer *Writer
	reader *Reader
}

// Connect opens an S3-compatible store and verifies the bucket is reachable.
// It returns an error rather than degrading: see NewMinioStore for why
// silently falling back to Postgres is the wrong failure mode here.
func Connect(ctx context.Context, opts Options) (*Store, error) {
	client, err := NewMinioStore(ctx, opts.Endpoint, opts.Region, opts.Bucket,
		opts.AccessKeyID, opts.SecretAccessKey, opts.PathStyle)
	if err != nil {
		return nil, err
	}
	return New(client, opts), nil
}

// New builds a Store over an already-open ObjectStore.
func New(client ObjectStore, opts Options) *Store {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "events"
	}
	return &Store{
		client: client,
		writer: NewWriter(client, prefix, WriterOptions{
			FlushInterval:   opts.FlushInterval,
			MaxBufferEvents: opts.MaxBufferEvents,
		}),
		reader: NewReader(client, ReaderOptions{
			Prefix:      prefix,
			ReadTimeout: opts.ReadTimeout,
			Concurrency: opts.Concurrency,
			Retention:   opts.Retention,
		}),
	}
}

// InsertEvents buffers a batch of ingested events. Implemented for
// handlers.EventSink; the method set is matched structurally, so this
// package does not have to import the HTTP layer.
func (s *Store) InsertEvents(ctx context.Context, events []dbpkg.Event) error {
	return s.writer.InsertEvents(ctx, events)
}

// Close flushes buffered events and stops the writer.
func (s *Store) Close() error { return s.writer.Close() }

// QueryEvents reads paginated events, newest first.
func (s *Store) QueryEvents(ctx context.Context, q dbpkg.EventQuery) ([]dbpkg.Event, error) {
	return s.reader.QueryEvents(ctx, q)
}

// EventByID reads one event.
func (s *Store) EventByID(ctx context.Context, id string) (*dbpkg.Event, error) {
	return s.reader.EventByID(ctx, id)
}

// ScanEvents streams a time range for the rollup worker.
func (s *Store) ScanEvents(ctx context.Context, start, end time.Time, filter dbpkg.ScanFilter, fn func(dbpkg.Event) error) error {
	return s.reader.ScanEvents(ctx, start, end, filter, fn)
}

// SourceName implements db.EventReader and db.EventScanner.
func (s *Store) SourceName() string { return "s3" }

// DeleteBefore drops whole hour buckets older than cutoff.
func (s *Store) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return s.reader.DeleteBefore(ctx, cutoff)
}

// ObjectCount reports how many objects and hour buckets the prefix holds.
func (s *Store) ObjectCount(ctx context.Context) (int, int, error) {
	return s.reader.ObjectCount(ctx)
}

// Pending reports buffered and dropped event counts.
func (s *Store) Pending() (int, int64) { return s.writer.Pending() }

// Describe renders a redacted one-line summary for startup logging. The
// credentials are never part of it.
func (o Options) Describe() string {
	return fmt.Sprintf("s3 bucket=%s prefix=%s endpoint=%s region=%s path_style=%t",
		o.Bucket, o.Prefix, o.Endpoint, o.Region, o.PathStyle)
}

// The store must satisfy every raw-event interface the service uses.
var (
	_ dbpkg.EventReader  = (*Store)(nil)
	_ dbpkg.EventScanner = (*Store)(nil)
)
