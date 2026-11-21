package s3store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Key layout:
//
//	<prefix>/dt=2026-11-21/hour=14/000000000123456789.ndjson.gz
//
// Time-major, newest partition last in the path, because the only listings
// this package performs are "everything in this hour" (a read) and
// "everything older than this hour" (retention). Both are prefix scans.
//
// The project is deliberately *not* a path segment. Putting it before the
// hour would make a cross-project time-range read impossible without first
// enumerating every project, and the dashboard's admin view does exactly
// that. Per-project retention is honoured at read time instead — see the
// package doc.

const keyTimeLayout = "2006-01-02"
const keyHourLayout = "15"

// hourPrefix is the listing prefix for one hour bucket.
func hourPrefix(prefix string, hour time.Time) string {
	h := hour.UTC()
	return fmt.Sprintf("%s/dt=%s/hour=%s/", prefix, h.Format(keyTimeLayout), h.Format(keyHourLayout))
}

// objectKey names one object. The first event's id makes it unique and
// roughly ordered; ids are unique by construction, so no random suffix is
// needed and two writers can never collide.
func objectKey(prefix string, hour time.Time, firstID uint) string {
	return fmt.Sprintf("%s%012d.ndjson.gz", hourPrefix(prefix, hour), firstID)
}

// parseObjectHour recovers the hour a key belongs to. It is used by the
// retention pass, which lists and deletes rather than reading.
func parseObjectHour(prefix, key string) (time.Time, bool) {
	rel := strings.TrimPrefix(key, prefix+"/")
	parts := strings.Split(rel, "/")
	var day, hour string
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "dt="):
			day = strings.TrimPrefix(p, "dt=")
		case strings.HasPrefix(p, "hour="):
			hour = strings.TrimPrefix(p, "hour=")
		}
	}
	if day == "" || hour == "" {
		return time.Time{}, false
	}
	dayText := strings.ReplaceAll(day, "_", "-")
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(keyTimeLayout, dayText, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t.Add(time.Duration(h) * time.Hour), true
}

// hoursBetween lists every hour bucket touching [start, end], newest first,
// capped at max hours so a nonsensical range cannot turn into an unbounded
// listing. A cap of <= 0 means no cap.
func hoursBetween(start, end time.Time, max int) []time.Time {
	if end.Before(start) {
		return nil
	}
	first := start.UTC().Truncate(time.Hour)
	last := end.UTC().Truncate(time.Hour)

	var hours []time.Time
	for h := last; !h.Before(first); h = h.Add(-time.Hour) {
		hours = append(hours, h)
		if max > 0 && len(hours) >= max {
			break
		}
	}
	return hours
}
