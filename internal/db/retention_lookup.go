package db

import (
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"
)

// retentionLookupTTL is how long a resolved project→window map is reused.
// Retention changes are an operator action taken from the console, so a
// minute of staleness costs nothing, while a lookup per event would put a
// query on the read path of every raw-event scan.
const retentionLookupTTL = 60 * time.Second

// ProjectRetentionLookup answers how long a project's events live.
//
// The object-store reader needs this for a reason the SQL stores do not
// have: a SQL store can delete and rewrite `expires_at` in place, but an
// object cannot be edited. So instead of stamping an expiry into the object
// and trusting it forever, the reader resolves the project's *current*
// window and drops events older than `now - window` as it reads them. That
// makes a shortened window take effect immediately — no rewrite, no
// backfill, and no window in which the store still serves rows the operator
// has already deleted.
type ProjectRetentionLookup interface {
	// ProjectRetentionDays returns the window in days for one project
	// namespace, falling back to the configured default when the project
	// (or its keys) defer to it.
	ProjectRetentionDays(userID, project string) int

	// DefaultRetentionDays is the configured APP_RETENTION_DAYS.
	DefaultRetentionDays() int

	// MaxRetentionDays is the longest window any project currently asks
	// for. Whole objects older than this can be deleted: no project could
	// still want them.
	MaxRetentionDays() int
}

// RetentionResolver resolves project retention from the api_keys table with a
// short-lived cache. It satisfies ProjectRetentionLookup.
type RetentionResolver struct {
	db          *gorm.DB
	defaultDays int
	ttl         time.Duration

	mu        sync.RWMutex
	byProject map[string]int
	maxDays   int
	loadedAt  time.Time
}

// NewRetentionResolver builds a resolver. defaultDays is APP_RETENTION_DAYS
// and is what a key with retention_days = 0 inherits.
func NewRetentionResolver(db *gorm.DB, defaultDays int) *RetentionResolver {
	return &RetentionResolver{
		db:          db,
		defaultDays: defaultDays,
		ttl:         retentionLookupTTL,
		byProject:   map[string]int{},
		maxDays:     defaultDays,
	}
}

// retentionKey namespaces a project by its owner: the same project name can
// exist under two users and the two namespaces are unrelated.
func retentionKey(userID, project string) string { return userID + "|" + project }

// refresh reloads the project→window map. It never returns an error to the
// caller: a failed reload keeps the previous snapshot, because serving
// slightly stale retention is strictly better than failing a dashboard read.
func (r *RetentionResolver) refresh() {
	if r == nil || r.db == nil {
		return
	}
	var rows []keyRetentionRow
	if err := r.db.Model(&APIKey{}).
		Select("user_id, name, retention_days").
		Scan(&rows).Error; err != nil {
		return
	}

	// Group the keys by the namespace they write into, then resolve each
	// namespace to the longest window its keys ask for.
	//
	// A namespace's window is computed with LongestRetentionWindow rather than
	// a `>` loop because 0 means unlimited: with `>`, an unlimited key reads as
	// the narrowest value in the group and loses to any finite sibling, so a
	// project set to keep everything would be served as if it kept a week.
	byProject, maxDays := resolveRetentionWindows(rows, r.defaultDays)

	r.mu.Lock()
	r.byProject = byProject
	r.maxDays = maxDays
	r.loadedAt = time.Now()
	r.mu.Unlock()
}

// keyRetentionRow is one API key as the resolver sees it.
type keyRetentionRow struct {
	UserID        uint
	Name          string
	RetentionDays int
}

// resolveRetentionWindows folds every key into the window its namespace
// honours, plus the widest window in play.
//
// The widest is what the object store uses to decide when a whole hour bucket
// is safe to delete: nothing may go while any project could still want it.
//
// Split out from the query so the fold can be tested without a database, which
// matters because getting it wrong is silent — the symptom is a project
// quietly served a shorter window than it was promised, not an error.
func resolveRetentionWindows(rows []keyRetentionRow, defaultDays int) (map[string]int, int) {
	grouped := make(map[string][]int, len(rows))
	for _, r := range rows {
		key := retentionKey(strconv.Itoa(int(r.UserID)), r.Name)
		grouped[key] = append(grouped[key], r.RetentionDays)
	}

	byProject := make(map[string]int, len(grouped))
	widest := defaultDays
	for key, days := range grouped {
		window := LongestRetentionWindow(defaultDays, days)
		byProject[key] = window
		if RetentionWider(window, widest) {
			widest = window
		}
	}
	return byProject, widest
}

// ensureFresh reloads the snapshot when it has aged out.
func (r *RetentionResolver) ensureFresh() {
	if r == nil {
		return
	}
	r.mu.RLock()
	stale := time.Since(r.loadedAt) > r.ttl
	r.mu.RUnlock()
	if stale {
		r.refresh()
	}
}

// ProjectRetentionDays implements ProjectRetentionLookup. An unknown project
// gets the default window: a project with no API key left still has stored
// events, and the safe reading of "no policy" is the global one.
//
// The stored value is returned as-is, including 0. It has already had the
// default applied to deferring keys, and 0 there means unlimited — turning it
// back into the default would resurrect a finite window for a project whose
// keys asked to keep everything.
func (r *RetentionResolver) ProjectRetentionDays(userID, project string) int {
	if r == nil {
		return 0
	}
	r.ensureFresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if days, ok := r.byProject[retentionKey(userID, project)]; ok {
		return days
	}
	return r.defaultDays
}

// DefaultRetentionDays implements ProjectRetentionLookup.
func (r *RetentionResolver) DefaultRetentionDays() int {
	if r == nil {
		return 0
	}
	return r.defaultDays
}

// MaxRetentionDays implements ProjectRetentionLookup.
//
// 0 is a real answer (unlimited) rather than a missing one, so it is returned
// as-is. Falling back to the default here would resurrect a finite window and
// start deleting objects a project had asked to keep forever.
func (r *RetentionResolver) MaxRetentionDays() int {
	if r == nil {
		return 0
	}
	r.ensureFresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxDays
}
