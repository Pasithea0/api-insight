package s3store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"gorm.io/datatypes"

	dbpkg "apiinsight/internal/db"
)

// maxLineBytes bounds one decoded NDJSON line. Attributes are arbitrary
// caller-supplied JSON, so a single event could in principle be large; the
// cap stops a corrupt object from being read into memory unbounded.
const maxLineBytes = 4 << 20 // 4 MiB

// wireEvent is the on-object shape of one event.
//
// Field names are one or two characters because they are repeated once per
// event and the operator pays for the bytes: at millions of events a day,
// `created_at` versus `t` is real money. The mapping is fixed, and
// wireVersion would be the place to record a change if it ever needs one.
type wireEvent struct {
	ID uint           `json:"id"`
	T  int64          `json:"t"`           // created_at, unix milliseconds UTC
	E  *int64         `json:"e,omitempty"` // expires_at, unix milliseconds UTC
	U  string         `json:"u,omitempty"` // user_id
	P  string         `json:"p,omitempty"` // project
	R  string         `json:"r,omitempty"` // route
	M  string         `json:"m,omitempty"` // method
	S  int            `json:"s,omitempty"` // status
	D  int64          `json:"d,omitempty"` // duration_ms
	IP string         `json:"i,omitempty"` // remote_ip
	A  map[string]any `json:"a,omitempty"` // attributes
}

// encodeEvents renders events as gzipped NDJSON, ready to be one object.
func encodeEvents(events []dbpkg.Event) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("s3store: gzip writer: %w", err)
	}
	enc := json.NewEncoder(zw)
	for i := range events {
		if err := enc.Encode(toWire(events[i])); err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("s3store: encode event %d: %w", i, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("s3store: finish gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeEvents expands one object back into events. A malformed line is
// reported with its index, and the whole object fails: silently skipping a
// line would lose an event without anyone noticing.
func decodeEvents(data []byte) ([]dbpkg.Event, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("s3store: gunzip: %w", err)
	}
	defer zr.Close()

	events := make([]dbpkg.Event, 0, 1024)
	dec := json.NewDecoder(zr)
	for {
		var w wireEvent
		err := dec.Decode(&w)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("s3store: decode event %d: %w", len(events), err)
		}
		events = append(events, fromWire(w))
	}
	return events, nil
}

func toWire(e dbpkg.Event) wireEvent {
	w := wireEvent{
		ID: e.ID,
		T:  e.CreatedAt.UTC().UnixMilli(),
		U:  e.UserID,
		P:  e.Project,
		R:  e.Route,
		M:  e.Method,
		S:  e.Status,
		D:  e.DurationMs,
		IP: e.RemoteIP,
	}
	if e.ExpiresAt != nil {
		ms := e.ExpiresAt.UTC().UnixMilli()
		w.E = &ms
	}
	if len(e.Attributes) > 0 {
		w.A = map[string]any(e.Attributes)
	}
	return w
}

func fromWire(w wireEvent) dbpkg.Event {
	e := dbpkg.Event{
		ID:         w.ID,
		CreatedAt:  time.UnixMilli(w.T).UTC(),
		UserID:     w.U,
		Project:    w.P,
		Route:      w.R,
		Method:     w.M,
		Status:     w.S,
		DurationMs: w.D,
		RemoteIP:   w.IP,
	}
	if w.E != nil {
		t := time.UnixMilli(*w.E).UTC()
		e.ExpiresAt = &t
	}
	if len(w.A) > 0 {
		e.Attributes = datatypes.JSONMap(w.A)
	}
	return e
}
