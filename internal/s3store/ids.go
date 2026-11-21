package s3store

import (
	"sync"
	"time"
)

// Event ids in the object store are generated here rather than by a database
// sequence, and they are shaped to satisfy three constraints at once.
//
//  1. They must survive JSON. The dashboard puts an event's id in a URL and
//     JavaScript parses JSON numbers as float64, so anything above 2^53
//     loses precision and `GET /v1/metrics/event/:id` would 404 on a
//     perfectly good row. A 63-bit snowflake is therefore not usable.
//
//  2. They must reveal their own timestamp. An object store has no index, so
//     the only way to find one event without scanning everything is to
//     derive which hour bucket it must live in.
//
//  3. They must be recognisable as ours. The reader is also asked about rows
//     written before the switch, whose ids came from a Postgres sequence, and
//     it has to answer "not mine" without a lookup.
//
// The layout is therefore, inside 53 bits:
//
//	[1 marker bit][40 time bits (ms since idEpoch)][12 sequence bits]
//
// The marker bit is what makes (3) exact rather than a guess: a small
// Postgres integer never sets it. The time field covers ~34 years from
// idEpoch, which is the price of having it at all, and the sequence handles a
// burst of 4096 events inside one millisecond.
const (
	// idEpochMs is 2024-01-01T00:00:00Z. An event older than this is a
	// backfill, not traffic, and is clamped onto the epoch.
	idEpochMs = 1704067200000

	idSeqBits  = 12
	idMaxSeq   = (1 << idSeqBits) - 1
	idTimeBits = 40
	idMaxMs    = (1 << idTimeBits) - 1

	// idMarker is bit 52 of the 53 usable bits: the highest bit that is
	// still exact in a JSON number.
	idMarker = uint(1) << 52
)

// IDGenerator hands out strictly increasing 53-bit event ids.
//
// Strictly increasing matters: two events in one ingest request share the
// same timestamp (the handler reads the clock once), and a backdated batch
// can pin thousands of events to one millisecond, so a generator that only
// had a timestamp would collide immediately. On overflow the sequence carries
// into the millisecond, which keeps ids unique and ordered even for a
// pathological burst.
type IDGenerator struct {
	mu     sync.Mutex
	lastMs int64
	seq    int64
}

// NewIDGenerator returns a generator starting from the caller's first
// timestamp.
func NewIDGenerator() *IDGenerator { return &IDGenerator{} }

// Next allocates an id for an event that occurred at `at`.
//
// Every returned id has the marker bit set, so it is never zero — which
// matters, because zero is the sentinel everywhere else in this codebase for
// "this event has no id yet". A backfill older than idEpoch would otherwise
// mint exactly zero.
func (g *IDGenerator) Next(at time.Time) uint {
	ms := at.UTC().UnixMilli() - idEpochMs
	if ms < 0 {
		// A backfill older than the epoch keeps the epoch: the id only has
		// to be unique and findable, not to reproduce the timestamp
		// exactly, and the object's own hour prefix is authoritative.
		ms = 0
	}
	if ms > idMaxMs {
		ms = idMaxMs
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case ms > g.lastMs:
		g.lastMs = ms
		g.seq = 0
	case g.seq >= idMaxSeq:
		// 4097 events in the same millisecond: borrow the next one. The
		// clock is not consulted again, so this stays monotonic even if the
		// borrow crosses an hour boundary.
		g.lastMs++
		g.seq = 0
	default:
		// Same millisecond, or time went backwards (an NTP step). Keep
		// counting forward from where we were: ids must never repeat.
		g.seq++
	}
	if g.lastMs > idMaxMs {
		// Past the end of the time field (~2058). The sequence keeps ids
		// unique; only the timestamp embedded in them stops advancing.
		g.lastMs = idMaxMs
		g.seq++
		if g.seq > idMaxSeq {
			g.seq = idMaxSeq
		}
	}

	return idMarker | uint(g.lastMs)<<idSeqBits | uint(g.seq)
}

// IDTime recovers the timestamp encoded in an id. ok is false when the id did
// not come from this generator, which is how the reader tells an object-store
// id from one a previous Postgres deployment handed out.
func IDTime(id uint) (time.Time, bool) {
	if id&idMarker == 0 {
		return time.Time{}, false
	}
	ms := int64((id >> idSeqBits) & idMaxMs)
	return time.UnixMilli(ms + idEpochMs).UTC(), true
}

// IDHour is the hour bucket an event with this id is stored under. It is the
// only reason the timestamp is embedded in the id at all.
func IDHour(id uint) (time.Time, bool) {
	t, ok := IDTime(id)
	if !ok {
		return time.Time{}, false
	}
	return t.Truncate(time.Hour), true
}
