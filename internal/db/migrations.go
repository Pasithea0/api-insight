package db

import (
	"log"

	"gorm.io/gorm"
)

// indexTarget groups an index definition with the table it belongs to.
type indexTarget struct {
	Name   string
	Table  string
	Column string
	Unique bool
}

// desiredIndexes lists all indexes we want to create across all tables.
var desiredIndexes = []indexTarget{
	// Events table — created CONCURRENTLY to avoid production locks
	{Name: "idx_events_created_at", Table: "events", Column: "(created_at)"},
	{Name: "idx_events_user_project_created_desc", Table: "events", Column: "(user_id, project, created_at DESC)"},
	{Name: "idx_events_user_project_route", Table: "events", Column: "(user_id, project, route)"},
	{Name: "idx_events_user_project_route_created", Table: "events", Column: "(user_id, project, route, created_at)"},

	// Metric bucket table — small, safe to create in-transaction
	{Name: "idx_metric_buckets_bucket_start", Table: "metric_buckets", Column: "(bucket_start)"},

	// Route bucket table — the query pattern is (user_id|project|bucket_start) for dashboard queries
	{Name: "idx_route_buckets_bucket_start", Table: "route_buckets", Column: "(bucket_start)"},
	{Name: "idx_route_buckets_user_project_start", Table: "route_buckets", Column: "(user_id, project, bucket_start)"},
}

// runMigrations applies schema changes that AutoMigrate cannot handle:
//   - New indexes created CONCURRENTLY where needed
//   - Drop indexes that were incorrectly created on the wrong table
//   - Autovacuum tuning for the events table
func runMigrations(db *gorm.DB) {
	dropMisplacedBucketIndexes(db)
	createIndexesConcurrently(db)
	tuneAutovacuum(db)
}

// dropMisplacedBucketIndexes removes any bucket-table indexes that were
// incorrectly created on the events table by an earlier buggy migration.
func dropMisplacedBucketIndexes(db *gorm.DB) {
	for _, name := range []string{"idx_metric_buckets_bucket_start", "idx_route_buckets_bucket_start", "idx_route_buckets_user_project_start"} {
		// Check if the index exists on the events table
		var onEvents int64
		db.Raw(`SELECT COUNT(*) FROM pg_index i
			JOIN pg_class idx ON idx.oid = i.indexrelid
			JOIN pg_class tbl ON tbl.oid = i.indrelid
			WHERE idx.relname = ? AND tbl.relname = 'events'`, name).Scan(&onEvents)
		if onEvents > 0 {
			log.Printf("migration: dropping misplaced index %s on events table", name)
			db.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + name)
		}
	}
}

// createIndexesConcurrently creates each desired index, using CONCURRENTLY
// for the events table to avoid locking, and simple CREATE for small tables.
func createIndexesConcurrently(db *gorm.DB) {
	for _, idx := range desiredIndexes {
		if indexExists(db, idx.Name) {
			continue
		}
		unique := ""
		if idx.Unique {
			unique = "UNIQUE "
		}

		useConcurrently := idx.Table == "events"
		concurrently := ""
		if useConcurrently {
			concurrently = "CONCURRENTLY "
		}

		sql := "CREATE " + unique + "INDEX " + concurrently + "IF NOT EXISTS " + idx.Name + " ON " + idx.Table + " " + idx.Column
		if err := db.Exec(sql).Error; err != nil {
			log.Printf("warning: could not create index %s on %s: %v", idx.Name, idx.Table, err)
		} else {
			log.Printf("created index %s on %s", idx.Name, idx.Table)
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

	// Also tune route_buckets since it's queried on every dashboard load
	db.Exec(`ALTER TABLE route_buckets SET (autovacuum_vacuum_scale_factor = 0.05)`)
	log.Println("tuned autovacuum for route_buckets table")
}