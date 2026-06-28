package db

import (
	"log"

	"gorm.io/gorm"
)

// desiredIndexes lists indexes we want on the events table.
// Using CONCURRENTLY to avoid locking the table on production datasets.
var desiredIndexes = []struct {
	Name   string
	Column string
	Unique bool
}{
	{Name: "idx_events_created_at", Column: "(created_at)"},
	{Name: "idx_events_user_project_created_desc", Column: "(user_id, project, created_at DESC)"},
	{Name: "idx_events_user_project_route", Column: "(user_id, project, route)"},
	{Name: "idx_events_user_project_route_created", Column: "(user_id, project, route, created_at)"},
	{Name: "idx_metric_buckets_bucket_start", Column: "(bucket_start)"},
	{Name: "idx_route_buckets_bucket_start", Column: "(bucket_start)"},
}

// runMigrations applies schema changes that AutoMigrate cannot handle:
//   - New indexes created CONCURRENTLY to avoid table locks
//   - Autovacuum tuning for the events table
func runMigrations(db *gorm.DB) {
	createIndexesConcurrently(db)
	tuneAutovacuum(db)
}

// createIndexesConcurrently creates each desired index using CONCURRENTLY
// if it doesn't already exist.
func createIndexesConcurrently(db *gorm.DB) {
	for _, idx := range desiredIndexes {
		if indexExists(db, idx.Name) {
			continue
		}
		unique := ""
		if idx.Unique {
			unique = "UNIQUE "
		}
		sql := "CREATE " + unique + "INDEX CONCURRENTLY IF NOT EXISTS " + idx.Name + " ON events " + idx.Column
		if err := db.Exec(sql).Error; err != nil {
			log.Printf("warning: could not create index %s concurrently: %v (may need to run outside transaction)", idx.Name, err)
		} else {
			log.Printf("created index %s", idx.Name)
		}
	}

	// Indexes on bucket tables (smaller, safe to do in-transaction)
	for _, idx := range []struct{ Name, Column string }{
		{"idx_metric_buckets_bucket_start", "metric_buckets (bucket_start)"},
		{"idx_route_buckets_bucket_start", "route_buckets (bucket_start)"},
	} {
		if !indexExists(db, idx.Name) {
			if err := db.Exec("CREATE INDEX IF NOT EXISTS " + idx.Name + " ON " + idx.Column).Error; err != nil {
				log.Printf("warning: could not create index %s: %v", idx.Name, err)
			}
		}
	}
}

// indexExists checks whether an index with the given name exists in the
// current database by querying pg_class.
func indexExists(db *gorm.DB, name string) bool {
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM pg_class WHERE relname = ?", name).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// tuneAutovacuum sets aggressive autovacuum parameters on the events table
// to keep the query planner healthy after mass deletions by the retention worker.
func tuneAutovacuum(db *gorm.DB) {
	db.Exec(`ALTER TABLE events SET (autovacuum_vacuum_scale_factor = 0.01)`)
	db.Exec(`ALTER TABLE events SET (autovacuum_vacuum_threshold = 1000)`)
	db.Exec(`ALTER TABLE events SET (autovacuum_analyze_scale_factor = 0.005)`)
	db.Exec(`ALTER TABLE events SET (autovacuum_analyze_threshold = 500)`)
	log.Println("tuned autovacuum for events table (scale_factor=0.01/0.005, threshold=1000/500)")
}