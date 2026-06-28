package handlers

import (
	"log"
	"time"

	"gorm.io/gorm"

	dbpkg "apiinsight/internal/db"
)

// BatchWriter receives event batches on a channel and writes them to the
// database using GORM's batched Create. This decouples the ingest HTTP
// handler from the DB write latency, preventing backpressure during traffic
// spikes.
type BatchWriter struct {
	input chan []dbpkg.Event
	done  chan struct{}
	db    *gorm.DB
}

// NewBatchWriter starts the background writer goroutine.
// bufferSize: max queued batches before the ingest handler blocks.
func NewBatchWriter(db *gorm.DB, bufferSize int) *BatchWriter {
	bw := &BatchWriter{
		input: make(chan []dbpkg.Event, bufferSize),
		done:  make(chan struct{}),
		db:    db,
	}
	go bw.loop()
	return bw
}

// Submit enqueues a batch of events for writing. Blocks if the buffer is full.
func (bw *BatchWriter) Submit(events []dbpkg.Event) {
	bw.input <- events
}

// Stop gracefully shuts down the writer, flushing remaining events.
func (bw *BatchWriter) Stop() {
	close(bw.input)
	<-bw.done
}

func (bw *BatchWriter) loop() {
	for batch := range bw.input {
		if err := bw.db.Create(&batch).Error; err != nil {
			log.Printf("batch writer: failed to persist %d events: %v", len(batch), err)
		}
	}
	close(bw.done)
}

// FlushInterval is how often the batch writer checks for pending events
// when the channel is idle (not used in this simple implementation since
// we write each batch as it arrives).
const FlushInterval = 500 * time.Millisecond