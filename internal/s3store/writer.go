package s3store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	dbpkg "apiinsight/internal/db"
)

// Writer turns the ingest stream into objects.
//
// It buffers events in memory, grouped by hour, and writes each group as a
// single gzipped object, so the object count is governed by
// FlushInterval rather than by the ingest rate. At ~150 events/s a 30s
// interval is ~2.9k objects/day; flushing per 2s batch would be ~43k/day, and
// B2 bills listing, so the difference is worth buffering for.
//
// Ordering contract with the batch writer: InsertEvents returns as soon as
// the events are in memory. It never performs I/O, so a slow bucket can
// never back up ingest. The cost is that the last FlushInterval of traffic
// is lost if the process is killed — the same exposure the Postgres batch
// writer already has with its 2s window, made larger by the interval.
type Writer struct {
	store  ObjectStore
	prefix string
	ids    *IDGenerator

	flushInterval time.Duration
	maxBuffer     int

	mu      sync.Mutex
	buf     map[time.Time][]dbpkg.Event
	count   int
	dropped int64

	flushCh chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// WriterOptions configures a Writer.
type WriterOptions struct {
	// FlushInterval is how long events accumulate before being written.
	FlushInterval time.Duration

	// MaxBufferEvents caps the in-memory buffer. Reaching it forces an
	// immediate flush.
	MaxBufferEvents int
}

// NewWriter starts the background flusher.
func NewWriter(store ObjectStore, prefix string, opts WriterOptions) *Writer {
	interval := opts.FlushInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	maxBuffer := opts.MaxBufferEvents
	if maxBuffer <= 0 {
		maxBuffer = 200000
	}

	w := &Writer{
		store:         store,
		prefix:        prefix,
		ids:           NewIDGenerator(),
		flushInterval: interval,
		maxBuffer:     maxBuffer,
		buf:           map[time.Time][]dbpkg.Event{},
		flushCh:       make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	go w.loop()
	return w
}

// InsertEvents implements handlers.EventSink: it buffers the batch, minting
// an id for every event that does not have one yet, and returns.
func (w *Writer) InsertEvents(_ context.Context, events []dbpkg.Event) error {
	if len(events) == 0 {
		return nil
	}

	w.mu.Lock()
	for i := range events {
		// Postgres assigned ids from a sequence; there is no sequence here,
		// so the writer is the authority. An id already set (a replay, or a
		// batch handed back after a failed flush) is left alone.
		if events[i].ID == 0 {
			events[i].ID = w.ids.Next(events[i].CreatedAt)
		}
		hour := events[i].CreatedAt.UTC().Truncate(time.Hour)
		w.buf[hour] = append(w.buf[hour], events[i])
		w.count++
	}
	over := w.count >= w.maxBuffer
	w.mu.Unlock()

	if over {
		select {
		case w.flushCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Pending reports how many events are buffered and how many have been
// dropped after repeated write failures. Both are for logging.
func (w *Writer) Pending() (int, int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count, w.dropped
}

// Close flushes what is buffered and stops the flusher. It is safe to call
// more than once.
func (w *Writer) Close() error {
	var err error
	w.once.Do(func() {
		close(w.stop)
		<-w.done
		err = w.flush(context.Background())
	})
	return err
}

func (w *Writer) loop() {
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()
	defer close(w.done)

	for {
		select {
		case <-ticker.C:
			w.flushLogged(context.Background())
		case <-w.flushCh:
			w.flushLogged(context.Background())
		case <-w.stop:
			return
		}
	}
}

func (w *Writer) flushLogged(ctx context.Context) {
	if err := w.flush(ctx); err != nil {
		logf("s3store: flush: %v", err)
	}
}

// flush writes every hour group as one object. Hour buckets are written
// oldest-first so that a partial failure leaves the newest data buffered —
// if it comes to dropping, the oldest is the least painful to lose.
//
// On failure the group goes back into the buffer to be retried on the next
// tick. Memory is bounded: past maxBuffer*4 buffered events the oldest
// groups are dropped outright, because an unbounded buffer turns "the bucket
// is unreachable" into an OOM kill, and a killed process loses everything,
// including the newest events.
func (w *Writer) flush(ctx context.Context) error {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return nil
	}
	pending := w.buf
	w.buf = map[time.Time][]dbpkg.Event{}
	w.count = 0
	w.mu.Unlock()

	hours := make([]time.Time, 0, len(pending))
	for h := range pending {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })

	var firstErr error
	for _, hour := range hours {
		events := pending[hour]
		if len(events) == 0 {
			continue
		}
		data, err := encodeEvents(events)
		if err != nil {
			w.requeue(hour, events)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		key := objectKey(w.prefix, hour, events[0].ID)
		if err := w.store.Put(ctx, key, data); err != nil {
			w.requeue(hour, events)
			logf("s3store: write of %d events for %s failed, keeping them buffered: %v",
				len(events), hour.Format(time.RFC3339), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}

	w.enforceBufferCeiling()
	return firstErr
}

func (w *Writer) requeue(hour time.Time, events []dbpkg.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf[hour] = append(w.buf[hour], events...)
	w.count += len(events)
}

// enforceBufferCeiling drops the oldest buffered hours once the buffer has
// grown past a hard multiple of the configured limit.
func (w *Writer) enforceBufferCeiling() {
	w.mu.Lock()
	defer w.mu.Unlock()

	ceiling := w.maxBuffer * 4
	if w.count <= ceiling {
		return
	}

	hours := make([]time.Time, 0, len(w.buf))
	for h := range w.buf {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })

	for _, h := range hours {
		if w.count <= w.maxBuffer {
			break
		}
		n := len(w.buf[h])
		delete(w.buf, h)
		w.count -= n
		w.dropped += int64(n)
		logf("s3store: dropped %d buffered events for %s: the object store has been "+
			"rejecting writes and the buffer is over its ceiling (%d events)", n,
			h.Format(time.RFC3339), ceiling)
	}
}

// ErrNoStore is returned by the package's exported helpers when nothing is
// configured.
var ErrNoStore = errors.New("s3store: no object store configured")
