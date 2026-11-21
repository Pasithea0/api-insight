package s3store

import (
	"context"
	"errors"
	"io"
	"sort"
	"time"
)

// retentionDeleteBatch bounds how many keys one delete request carries.
const retentionDeleteBatch = 1000

// DeleteBefore removes every object whose hour bucket is entirely older than
// cutoff, and reports how many objects were removed.
//
// The caller must pass a cutoff derived from the *longest* window any
// project asks for, because an object cannot be split: if one project keeps
// 30 days and another 2, the hour buckets in between still hold the first
// project's events and deleting them early would destroy data that project
// was promised. Per-project windows are enforced at read time instead, which
// is why a shortened window does not need this pass to run at all.
func (r *Reader) DeleteBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if r.store == nil {
		return 0, ErrNoStore
	}
	prefix := r.opts.Prefix + "/"
	if err := mustPrefix(prefix); err != nil {
		return 0, err
	}

	// One listing of the whole prefix, then delete the hour groups that are
	// old enough. Listing is recursive and paginated by the client; the
	// alternative (walking hour prefixes one by one) would issue an order of
	// magnitude more list calls for the same answer.
	var doomed []string
	err := r.store.List(ctx, prefix, func(key string) error {
		hour, ok := parseObjectHour(r.opts.Prefix, key)
		if !ok {
			// Unrecognised key: leave it alone rather than guess. It is
			// either a foreign object under our prefix or a layout from a
			// future version.
			return nil
		}
		// Whole buckets only: an object is deleted only when its entire
		// hour is outside the window.
		if hour.Add(time.Hour).Before(cutoff) || hour.Add(time.Hour).Equal(cutoff) {
			doomed = append(doomed, key)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(doomed) == 0 {
		return 0, nil
	}

	sort.Strings(doomed)
	removed := 0
	for start := 0; start < len(doomed); start += retentionDeleteBatch {
		end := start + retentionDeleteBatch
		if end > len(doomed) {
			end = len(doomed)
		}
		batch := doomed[start:end]
		if err := r.store.Delete(ctx, batch); err != nil {
			return removed, err
		}
		removed += len(batch)
	}
	return removed, nil
}

// ObjectCount reports how many objects live under the prefix and how many
// hours they span. It exists for startup logging: an operator switching a
// live deployment needs to see that the bucket is not empty before they
// trust it.
func (r *Reader) ObjectCount(ctx context.Context) (objects int, hours int, err error) {
	if r.store == nil {
		return 0, 0, ErrNoStore
	}
	prefix := r.opts.Prefix + "/"
	if err := mustPrefix(prefix); err != nil {
		return 0, 0, err
	}
	seen := map[string]struct{}{}
	err = r.store.List(ctx, prefix, func(key string) error {
		objects++
		if hour, ok := parseObjectHour(r.opts.Prefix, key); ok {
			seen[hour.Format(time.RFC3339)] = struct{}{}
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return objects, len(seen), err
	}
	return objects, len(seen), nil
}
