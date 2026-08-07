package handlers

import (
	"log"
	"sync"
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

// BatchWriter accumulates ingested events in memory and writes them to
// the database in periodic multi-row INSERTs. This decouples the ingest
// HTTP handler from DB write latency AND amortizes writes: at N events/s
// we issue one INSERT per FlushInterval instead of one per request.
type BatchWriter struct {
	db *gorm.DB

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

// Stop flushes any remaining buffered events and shuts down the writer.
func (bw *BatchWriter) Stop() {
	close(bw.stop)
	<-bw.done
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
	}
}
