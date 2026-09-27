package db

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// PostgresReader serves raw-event reads from the system of record.
//
// This reproduces the SQL the metrics handlers used to build inline, so
// routing a request through the EventReader abstraction is behaviour
// preserving: same filters, same ordering, same pagination.
type PostgresReader struct {
	db *gorm.DB
}

// NewPostgresReader wraps a GORM handle as an EventReader.
func NewPostgresReader(db *gorm.DB) *PostgresReader {
	return &PostgresReader{db: db}
}

// SourceName implements EventReader.
func (r *PostgresReader) SourceName() string { return "postgres" }

// QueryEvents implements EventReader.
func (r *PostgresReader) QueryEvents(ctx context.Context, q EventQuery) ([]Event, error) {
	query := r.db.WithContext(ctx).Model(&Event{}).Where("created_at >= ?", q.Cutoff)

	if q.UserID != "" {
		query = query.Where("user_id = ?", q.UserID)
	}
	if q.Project != "" {
		query = query.Where("project = ?", q.Project)
	}
	switch q.Status {
	case "success":
		query = query.Where("status < ?", 400)
	case "error":
		query = query.Where("status >= ?", 400)
	}
	if q.AttrKey != "" && q.AttrValue != "" && SafeAttrKey.MatchString(q.AttrKey) {
		// @> hits the GIN index rather than scanning the jsonb column.
		query = query.Where("attributes @> CAST(json_build_object(?, ?) AS jsonb)", q.AttrKey, q.AttrValue)
	}
	if q.Field != "" && q.Pattern != "" {
		expr, ok := PostgresFieldExpr(q.Field)
		if !ok {
			return nil, fmt.Errorf("invalid search field %q", q.Field)
		}
		query = query.Where(expr+" LIKE ?", q.Pattern)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	var events []Event
	if err := query.
		Order("created_at DESC").
		Limit(limit).
		Offset(q.Offset).
		Find(&events).Error; err != nil {
		return nil, err
	}
	return events, nil
}

// EventByID implements EventReader.
func (r *PostgresReader) EventByID(ctx context.Context, id string) (*Event, error) {
	var event Event
	// Find with a string primary key leaves event.ID zero when absent, which
	// the caller treats as not-found (matching the previous behaviour).
	if err := r.db.WithContext(ctx).
		Where("id = ?", id).
		Limit(1).
		Find(&event).Error; err != nil {
		return nil, err
	}
	if event.ID == 0 {
		return nil, nil
	}
	return &event, nil
}
