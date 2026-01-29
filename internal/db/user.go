package db

import (
	"time"
)

// User represents a dashboard user that can sign in to the UI and
// own API keys / metrics. The bootstrap admin user (from env) will
// be created as a row in this table on startup.
type User struct {
	ID uint `gorm:"primaryKey"`

	CreatedAt time.Time
	UpdatedAt time.Time

	Username     string `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string `gorm:"size:255;not null"`

	// IsAdmin marks users that can manage other users and global
	// settings. The bootstrap admin will have IsAdmin=true.
	IsAdmin bool `gorm:"default:false"`

	// TimeFormat: "12" = 12-hour, "24" = 24-hour. Default "12".
	TimeFormat string `gorm:"size:8;default:12"`
	// DateFormat: "dd-mm-yyyy", "mm-dd-yyyy", "yyyy-mm-dd". Default "dd-mm-yyyy".
	DateFormat string `gorm:"size:16;default:dd-mm-yyyy"`
}
