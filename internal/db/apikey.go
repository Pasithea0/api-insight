package db

import (
	"time"
)

// APIKey represents an API key for ingesting events from external services.
// Each key belongs to a user and has a name and environment (production type).
type APIKey struct {
	ID uint `gorm:"primaryKey"`

	CreatedAt time.Time
	UpdatedAt time.Time

	// UserID links this key to the user who owns it.
	UserID uint `gorm:"index;not null"`

	// Name is a user-friendly identifier for this key (e.g. "payments-api").
	Name string `gorm:"size:128;not null"`

	// Environment indicates the deployment environment (prod, staging, dev).
	Environment string `gorm:"size:32;not null"`

	// Key is the actual bearer token value (stored as-is, should be unique).
	Key string `gorm:"uniqueIndex;size:255;not null"`

	// RetentionDays is the number of days events ingested with this key
	// should be retained for. A value of 0 means "use the global default"
	// from config.
	RetentionDays int `gorm:"not null;default:0"`

	// Active indicates whether this key is currently enabled.
	Active bool `gorm:"default:true"`

	// User is the owner of this API key.
	User User `gorm:"foreignKey:UserID"`
}
