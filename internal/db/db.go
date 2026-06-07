package db

import (
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"apiinsight/internal/config"
)

// Connect opens a GORM database connection using APP_DATABASE_URL (PostgreSQL URL).
func Connect(cfg *config.Config) (*gorm.DB, error) {
	dsn := strings.TrimSpace(cfg.DatabaseURL)
	if dsn == "" {
		return nil, errors.New("APP_DATABASE_URL is required (PostgreSQL URL)")
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, errors.New("APP_DATABASE_URL must be a postgres:// or postgresql:// URL")
	}

	// PrepareStmt: true prevents the GORM postgres migrator from forcing simple protocol
	// for "SELECT * FROM table LIMIT 1", which would otherwise trigger "insufficient arguments".
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		PrepareStmt: true,
		// Batch multiple inserts into a single query to reduce round-trips and improve performance.
		CreateBatchSize: 100,
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// Tune connection pool for high throughput with 7.9M+ records.
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(20)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	// Auto-migrate the core tables.
	if err := db.AutoMigrate(&Event{}, &User{}, &APIKey{}, &MetricBucket{}, &RouteBucket{}, &AttributeKeyIndex{}); err != nil {
		return nil, err
	}

	// Ensure the created_at-only index exists for queries without a user_id filter (admin view).
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_events_created_at ON events (created_at)").Error; err != nil {
		return nil, err
	}

	return db, nil
}

// EnsureBootstrapAdmin makes sure there is at least one admin user
// corresponding to the bootstrap credentials in config. If a user with
// that username already exists, it is left as-is.
func EnsureBootstrapAdmin(db *gorm.DB, cfg *config.Config) error {
	if cfg.AdminUser == "" || cfg.AdminPassword == "" {
		return nil
	}

	var count int64
	if err := db.Model(&User{}).Where("username = ?", cfg.AdminUser).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	admin := &User{
		Username:     cfg.AdminUser,
		PasswordHash: string(hash),
		IsAdmin:      true,
	}

	return db.Create(admin).Error
}

// EnsureBootstrapAPIKey ensures the bootstrap admin user has a default API key
// matching the internal API key from config. This key is used for self-reporting.
// If the key already exists but is owned by a different user, it will be updated to belong to admin.
func EnsureBootstrapAPIKey(db *gorm.DB, cfg *config.Config) error {
	if cfg.InternalAPIKey == "" {
		return nil
	}

	var admin User
	if err := db.Where("username = ?", cfg.AdminUser).First(&admin).Error; err != nil {
		return err
	}

	// Check if API key already exists (use Find so "not found" doesn't log as error).
	var existingKey APIKey
	if err := db.Where("key = ?", cfg.InternalAPIKey).Limit(1).Find(&existingKey).Error; err == nil && existingKey.ID != 0 {
		// Key exists - ensure it belongs to admin.
		if existingKey.UserID != admin.ID {
			existingKey.UserID = admin.ID
			existingKey.Name = "api-insight"
			existingKey.Environment = "internal"
			existingKey.Active = true
			return db.Save(&existingKey).Error
		}
		// Already belongs to admin, nothing to do.
		return nil
	}

	// Key doesn't exist - create it for admin.
	apiKey := &APIKey{
		UserID:        admin.ID,
		Name:          "api-insight",
		Environment:   "internal",
		Key:           cfg.InternalAPIKey,
		Active:        true,
		RetentionDays: cfg.RetentionDays,
	}

	return db.Create(apiKey).Error
}
