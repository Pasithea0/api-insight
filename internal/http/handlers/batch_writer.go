package handlers

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

// FlushInterval is how often the batch writer drains buffered events
// into the database when the buffer hasn't reached MaxBatchSize yet.
// Events accumulate in memory for at most this long before being
// persisted in one multi-row INSERT.
const FlushInterval = 2 * time.Second

// MaxBatchSize is the maximum number of events buffered in memory before
// an immediate flush is triggered. It stays under PostgreSQL's parameter
// limit for a single multi-row INSERT (~65535 params / ~11 columns).
const MaxBatchSize = 5000

// EventSink is an optional secondary destination for ingested events.
//
// Postgres is always the system of record. A sink is best-effort: it is
// mirrored to asynchronously and a sink failure is logged, never propagated
// to the ingest caller and never allowed to fail a Postgres write.
type EventSink interface {
	InsertEvents(ctx context.Context, events []dbpkg.Event) error
	Close() error
}

// maxInflightSinkWrites caps concurrent mirror writes. Without it, a slow or
// unreachable secondary store would either block the flush loop (if called
// synchronously) or spawn unbounded goroutines (if called freely). Beyond
// the cap, batches are dropped and logged — acceptable, because Postgres
// already holds the data and the mirror can be backfilled.
const maxInflightSinkWrites = 4

// BatchWriter accumulates ingested events in memory and writes them to
// the database in periodic multi-row INSERTs. This decouples the ingest
// HTTP handler from DB write latency AND amortizes writes: at N events/s
// we issue one INSERT per FlushInterval instead of one per request.
type BatchWriter struct {
	db *gorm.DB

	sink     EventSink
	inflight atomic.Int64

	mu      sync.Mutex
	pending []dbpkg.Event

	flushCh chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// NewBatchWriter starts the background writer goroutine.
// bufferSize is retained for API compatibility; batching is governed by
// FlushInterval and MaxBatchSize.
func NewBatchWriter(db *gorm.DB, bufferSize int) *BatchWriter {
	bw := &BatchWriter{
		db:      db,
		pending: make([]dbpkg.Event, 0, MaxBatchSize),
		flushCh: make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go bw.loop()
	return bw
}

// Submit appends events to the in-memory buffer. It never blocks on the
// database; if the buffer reaches MaxBatchSize it signals an immediate
// flush so memory stays bounded.
func (bw *BatchWriter) Submit(events []dbpkg.Event) {
	bw.mu.Lock()
	bw.pending = append(bw.pending, events...)
	over := len(bw.pending) >= MaxBatchSize
	bw.mu.Unlock()

	if over {
		select {
		case bw.flushCh <- struct{}{}:
		default: // a flush is already pending; the ticker will drain it
		}
	}
}

// SetSink installs an optional secondary destination for ingested events.
// It must be called before the first Submit (i.e. during startup), which is
// how main wires it.
func (bw *BatchWriter) SetSink(sink EventSink) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	bw.sink = sink
}

// Stop flushes any remaining buffered events, waits briefly for in-flight
// mirror writes, and shuts down the writer. Waiting matters: the caller
// closes the sink right after this returns, and a mirror write landing in a
// closed client would panic. The wait is bounded so a wedged sink cannot
// hold up shutdown.
func (bw *BatchWriter) Stop() {
	close(bw.stop)
	<-bw.done

	deadline := time.Now().Add(10 * time.Second)
	for bw.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := bw.inflight.Load(); n > 0 {
		log.Printf("batch writer: %d sink write(s) still in flight at shutdown; abandoning them", n)
	}
}

func (bw *BatchWriter) loop() {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	defer close(bw.done)

	for {
		select {
		case <-ticker.C:
			bw.flush()
		case <-bw.flushCh:
			bw.flush()
		case <-bw.stop:
			bw.flush()
			return
		}
	}
}

// flush persists all buffered events in a single batched Create.
// On failure the batch is re-queued so a transient connection blip
// does not lose data.
func (bw *BatchWriter) flush() {
	bw.mu.Lock()
	if len(bw.pending) == 0 {
		bw.mu.Unlock()
		return
	}
	batch := bw.pending
	bw.pending = make([]dbpkg.Event, 0, MaxBatchSize)
	bw.mu.Unlock()

	if err := bw.db.Create(&batch).Error; err != nil {
		log.Printf("batch writer: failed to persist %d events: %v", len(batch), err)
		bw.mu.Lock()
		bw.pending = append(bw.pending, batch...)
		bw.mu.Unlock()
		return
	}

	// Mirror to the secondary sink only after Postgres has accepted the
	// batch, so the mirror can never hold data Postgres does not.
	bw.mirror(batch)
}

// mirror forwards a persisted batch to the configured sink without ever
// blocking the flush loop. See maxInflightSinkWrites for the back-pressure
// policy.
func (bw *BatchWriter) mirror(batch []dbpkg.Event) {
	bw.mu.Lock()
	sink := bw.sink
	bw.mu.Unlock()
	if sink == nil {
		return
	}

	if bw.inflight.Add(1) > maxInflightSinkWrites {
		bw.inflight.Add(-1)
		log.Printf("batch writer: sink is behind, dropping %d events from the mirror (Postgres has them)", len(batch))
		return
	}

	// The batch slice is freshly allocated by flush() and no longer shared,
	// so handing it to the goroutine is safe.
	go func() {
		defer bw.inflight.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sink.InsertEvents(ctx, batch); err != nil {
			log.Printf("batch writer: sink rejected %d events: %v", len(batch), err)
		}
	}()
}
