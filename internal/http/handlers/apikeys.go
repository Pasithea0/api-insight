package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"

	"apiinsight/internal/config"
	dbpkg "apiinsight/internal/db"
)

func generateToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.URLEncoding.EncodeToString(b), nil
}

func generateAPIKey() (string, error) {
	return generateToken("ai_")
}

func generatePublicAPIKey() (string, error) {
	return generateToken("aipub_")
}

func rotatePublicAPIKey() (string, error) {
	return generatePublicAPIKey()
}

func formOrQuery(ctx *fiber.Ctx, key string) string {
	if value := ctx.FormValue(key); value != "" {
		return value
	}
	return ctx.Query(key)
}

func CreateAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		name := ctx.FormValue("name")
		environment := ctx.FormValue("environment")
		retentionStr := ctx.FormValue("retention_days")

		if name == "" || environment == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("name and environment required")
		}

		maxRetention := cfg.RetentionDays
		retentionDays := maxRetention
		if retentionStr != "" {
			if v, err := strconv.Atoi(retentionStr); err == nil && v > 0 {
				if v > maxRetention {
					retentionDays = maxRetention
				} else {
					retentionDays = v
				}
			} else {
				return ctx.Status(fiber.StatusBadRequest).SendString("invalid retention_days")
			}
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		key, err := generateAPIKey()
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to generate API key")
		}
		publicKey, err := generatePublicAPIKey()
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to generate public API key")
		}

		apiKey := &dbpkg.APIKey{
			UserID:        user.ID,
			Name:          name,
			Environment:   environment,
			Key:           key,
			PublicKey:     publicKey,
			Active:        true,
			RetentionDays: retentionDays,
		}

		if err := db.Create(apiKey).Error; err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString("failed to create API key (name may already exist for this user)")
		}

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func DeleteAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := ctx.Query("id")
		if id == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}

		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		if cfg.InternalAPIKey != "" && apiKey.Key == cfg.InternalAPIKey {
			return ctx.Status(fiber.StatusForbidden).SendString("cannot delete internal API key")
		}

		if err := db.Delete(&apiKey).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to delete API key")
		}

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func SetActiveAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := formOrQuery(ctx, "id")
		activeStr := formOrQuery(ctx, "active")
		if id == "" || (activeStr != "true" && activeStr != "false") {
			return ctx.Status(fiber.StatusBadRequest).SendString("id and active (true|false) required")
		}
		active := activeStr == "true"

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		if err := db.Model(&apiKey).Update("active", active).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update API key")
		}
		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func SetPublicAPIKeyState(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := formOrQuery(ctx, "id")
		enabledStr := formOrQuery(ctx, "enabled")
		if id == "" || (enabledStr != "true" && enabledStr != "false") {
			return ctx.Status(fiber.StatusBadRequest).SendString("id and enabled (true|false) required")
		}
		enabled := enabledStr == "true"

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		updates := map[string]any{
			"public_enabled": enabled,
		}
		if enabled && apiKey.PublicKey == "" {
			publicKey, err := generatePublicAPIKey()
			if err != nil {
				return ctx.Status(fiber.StatusInternalServerError).SendString("failed to generate public API key")
			}
			updates["public_key"] = publicKey
		}

		if err := db.Model(&apiKey).Updates(updates).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update public API key")
		}
		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

func RotatePublicAPIKey(db *gorm.DB, cfg *config.Config) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := formOrQuery(ctx, "id")
		if id == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		publicKey, err := rotatePublicAPIKey()
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to rotate public API key")
		}

		if err := db.Model(&apiKey).Update("public_key", publicKey).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to rotate public API key")
		}
		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

// RetentionMirror is the optional secondary store's half of a retention edit.
//
// Postgres is the system of record, so mirroring an edit is best-effort: a nil
// mirror (ClickHouse disabled) is the normal case, and a mirror error is logged
// and dropped rather than failing the edit. The mirror must enforce the same
// window, though, because it can be the store the raw-event endpoints read from.
type RetentionMirror interface {
	SyncProjectRetention(ctx context.Context, userID, project string, retentionDays int) error
}

// retentionMirrorTimeout bounds one mirror reconciliation. ClickHouse mutations
// are asynchronous, so this bounds the request, not the rewrite.
const retentionMirrorTimeout = 60 * time.Second

// retentionPassMu serialises background reconciliation passes. A shrink can
// issue large batched DELETEs against `events`, the busiest table in the
// system; letting a burst of console edits run several of those at once would
// multiply the load for no gain.
var retentionPassMu sync.Mutex

// parseRetentionDays validates the retention field of the settings form.
//
// Empty or 0 means "use the configured default" — the same sentinel API keys
// already store — and a value above maxDays is clamped to it, matching what key
// creation does, so the console can never persist a window the service is not
// allowed to honour. Anything negative or non-numeric is rejected instead of
// guessed at: a typo must not silently become a retention policy.
func parseRetentionDays(raw string, maxDays int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return maxDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 0 {
		return 0, errors.New("invalid retention_days")
	}
	if days == 0 {
		return maxDays, nil
	}
	if maxDays > 0 && days > maxDays {
		return maxDays, nil
	}
	return days, nil
}

// SetAPIKeyRetention changes a project's retention window and reconciles the
// events already stored for it.
//
// The response is sent before the reconciliation happens. That is deliberate:
// shortening a window can delete millions of rows, and the operator's request
// must not be held open for that (the console is waiting on a redirect). The
// window itself is saved synchronously — so what the next ingest stamps is
// correct immediately — and the backlog is cleaned up in the background, with
// the outcome logged.
func SetAPIKeyRetention(db *gorm.DB, cfg *config.Config, mirror RetentionMirror) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		id := formOrQuery(ctx, "id")
		if id == "" {
			return ctx.Status(fiber.StatusBadRequest).SendString("id required")
		}
		days, err := parseRetentionDays(formOrQuery(ctx, "retention_days"), cfg.RetentionDays)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).SendString(err.Error())
		}

		user, ok := MustUser(ctx)
		if !ok {
			return nil
		}
		var apiKey dbpkg.APIKey
		if err := db.First(&apiKey, id).Error; err != nil {
			return ctx.Status(fiber.StatusNotFound).SendString("API key not found")
		}
		if apiKey.UserID != user.ID && !user.IsAdmin {
			return ctx.Status(fiber.StatusForbidden).SendString("forbidden")
		}

		if err := db.Model(&apiKey).Update("retention_days", days).Error; err != nil {
			return ctx.Status(fiber.StatusInternalServerError).SendString("failed to update retention")
		}

		// The window actually enforced is the project's, not this key's: keys
		// sharing a name share one event namespace, so a sibling key with a
		// longer window wins (see db.EffectiveProjectRetention).
		window, err := dbpkg.EffectiveProjectRetention(db, apiKey.UserID, apiKey.Name, cfg.RetentionDays)
		if err != nil {
			log.Printf("retention: could not resolve the window for %d/%s, reconciling to %d days: %v",
				apiKey.UserID, apiKey.Name, days, err)
			window = days
		}

		applyProjectRetentionAsync(db, mirror, strconv.Itoa(int(apiKey.UserID)), apiKey.Name, window)

		return ctx.Redirect("/settings", fiber.StatusSeeOther)
	}
}

// applyProjectRetentionAsync reconciles stored events with a project's new
// window without blocking the request that asked for it.
//
// Failures are logged, never surfaced: the setting is already saved and the
// daily retention worker keeps enforcing expiries, so a failed pass degrades to
// "the backlog goes on the next worker run" rather than losing the edit.
func applyProjectRetentionAsync(db *gorm.DB, mirror RetentionMirror, userID, project string, retentionDays int) {
	go func() {
		// A panic in a background goroutine takes the whole process with it,
		// and this one runs after the operator's edit has already been saved.
		// Containing it keeps a cleanup failure from becoming an outage.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("retention: cleanup of %s/%s panicked (setting saved): %v", userID, project, r)
			}
		}()

		retentionPassMu.Lock()
		defer retentionPassMu.Unlock()

		start := time.Now()
		res, err := dbpkg.ApplyProjectRetention(db, userID, project, retentionDays)
		if err != nil {
			log.Printf("retention: cleanup of %s/%s failed (setting saved; the worker will retry): %v",
				userID, project, err)
			return
		}
		log.Printf("retention: %s/%s now %d days — deleted %d expired events, rewrote %d expiries in %s",
			userID, project, retentionDays, res.Deleted, res.Rewritten, time.Since(start).Round(time.Millisecond))

		if mirror == nil {
			return
		}
		mctx, cancel := context.WithTimeout(context.Background(), retentionMirrorTimeout)
		defer cancel()
		if err := mirror.SyncProjectRetention(mctx, userID, project, retentionDays); err != nil {
			log.Printf("retention: mirror sync of %s/%s failed (Postgres is already updated): %v",
				userID, project, err)
		}
	}()
}
