package db

import (
	"time"

	"gorm.io/gorm"
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

	// PublicKey is a separate token that can be used for read-only public APIs.
	PublicKey string `gorm:"size:255"`

	// PublicEnabled controls whether public API access is allowed for this project.
	PublicEnabled bool `gorm:"default:false"`

	// RetentionDays is the number of days events ingested with this key
	// should be retained for. A value of 0 means "use the global default"
	// from config.
	RetentionDays int `gorm:"not null;default:0"`

	// Active indicates whether this key is currently enabled.
	Active bool `gorm:"default:true"`

	// User is the owner of this API key.
	User User `gorm:"foreignKey:UserID"`
}

// EffectiveProjectRetention returns the retention window, in days, that a
// project's event namespace has to honour given every key that writes into it.
//
// Events are stamped with the key's NAME, not its id (see IngestHandler), so
// two keys that share a name — the usual case being one project in prod and
// staging — share ONE event namespace while each carries its own
// RetentionDays. Editing one of them must therefore never shorten the
// namespace to that single key's window: a sibling key may still have promised
// to keep those rows longer, and reconciling to the shorter value would delete
// data the operator never asked to drop. The window enforced is the longest one
// any key with that name still asks for, with fallback standing in for keys
// that defer to the configured default (0).
func EffectiveProjectRetention(db *gorm.DB, userID uint, project string, fallback int) (int, error) {
	var keys []APIKey
	if err := db.Select("retention_days").
		Where("user_id = ? AND name = ?", userID, project).
		Find(&keys).Error; err != nil {
		return 0, err
	}

	window := 0
	for _, k := range keys {
		eff := k.RetentionDays
		if eff <= 0 {
			eff = fallback
		}
		if eff > window {
			window = eff
		}
	}
	if window <= 0 {
		window = fallback
	}
	return window, nil
}
