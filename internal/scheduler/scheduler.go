// Package scheduler runs the recurring maintenance jobs.

// Each job is independently enabled and guarded, and safe to run at any time: they are TTL
// sweeps and idempotent scans. One that overruns its interval is skipped rather than stacked,
// which keeps a slow scan from forking a second run every six hours.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/media"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
	"animind-backend-go/internal/storage"
	"animind-backend-go/internal/syncplay"
)

// Jobs bundles the periodic work with its guards.
type Jobs struct {
	cfg            *config.Config
	logger         *slog.Logger
	cron           *cron.Cron
	scanner        *scanner.Service
	hub            *syncplay.Hub
	audio          *media.AudioCache
	store          *repository.Store
	storageService *storage.Service

	mu      sync.Mutex
	running map[string]bool
}

// New builds the scheduler.
func New(cfg *config.Config, logger *slog.Logger, store *repository.Store, storageService *storage.Service, scannerService *scanner.Service, hub *syncplay.Hub, audio *media.AudioCache) *Jobs {
	return &Jobs{
		cfg: cfg, logger: logger, store: store, storageService: storageService,
		scanner: scannerService, hub: hub, audio: audio,
		cron: cron.New(cron.WithLogger(cron.DiscardLogger)), running: map[string]bool{},
	}
}

// Start registers every enabled job and starts the clock.
func (j *Jobs) Start() {
	if j.cfg.Scanner.Enabled && j.cfg.Scanner.Cron != "" {
		if _, err := j.cron.AddFunc(j.cfg.Scanner.Cron, func() { j.run("scanner", j.scanJob) }); err != nil {
			j.logger.Warn("invalid SCANNER_CRON expression; scheduled scans are disabled",
				slog.String("expression", j.cfg.Scanner.Cron), slog.String("error", err.Error()))
		} else {
			j.logger.Info("scheduled library scans", slog.String("cron", j.cfg.Scanner.Cron))
		}
	}

	if j.cfg.SyncPlay.CleanupEnabled && j.cfg.SyncPlay.Enabled {
		if _, err := j.cron.AddFunc(j.cfg.SyncPlay.CleanupCron, func() { j.run("syncplay_cleanup", j.syncPlayCleanupJob) }); err != nil {
			j.logger.Warn("invalid SYNCPLAY_CLEANUP_CRON expression", slog.String("expression", j.cfg.SyncPlay.CleanupCron))
		} else {
			j.logger.Info("scheduled watch party cleanup",
				slog.String("cron", j.cfg.SyncPlay.CleanupCron),
				slog.String("ttl", j.cfg.SyncPlay.EndedTTL.String()))
		}
	}

	if j.cfg.AudioCache.CleanupEnabled && j.cfg.IsLocal() {
		if _, err := j.cron.AddFunc(j.cfg.AudioCache.CleanupCron, func() { j.run("audio_cache_cleanup", j.audioCleanupJob) }); err != nil {
			j.logger.Warn("invalid AUDIO_CACHE_VARIANT_CLEANUP_CRON expression",
				slog.String("expression", j.cfg.AudioCache.CleanupCron))
		} else {
			j.logger.Info("scheduled audio variant cache cleanup",
				slog.String("cron", j.cfg.AudioCache.CleanupCron), slog.String("ttl", j.cfg.AudioCache.VariantTTL.String()))
		}
	}

	if j.cfg.Auth.CleanupEnabled {
		if _, err := j.cron.AddFunc(j.cfg.Auth.CleanupCron, func() { j.run("auth_cleanup", j.authCleanupJob) }); err != nil {
			j.logger.Warn("invalid DESKTOP_AUTH_CLEANUP_CRON expression", slog.String("expression", j.cfg.Auth.CleanupCron))
		} else {
			j.logger.Info("scheduled session cleanup",
				slog.String("cron", j.cfg.Auth.CleanupCron), slog.String("retention", j.cfg.Auth.ExpiredRetention.String()))
		}
	}

	j.cron.Start()

	// Boot passes run immediately so a restarted server does not wait for a schedule to
	// reclaim dead sessions, stale rooms or expired transcodes.
	go j.run("syncplay_startup", j.syncPlayCleanupJob)
	go j.run("audio_cache_startup", j.audioCleanupJob)
	go j.run("auth_startup", j.authCleanupJob)

	if j.cfg.Scanner.ScanOnStartup && j.cfg.Scanner.Enabled {
		go j.run("scanner_startup", j.scanJob)
	}
}

// Stop halts the clock and waits for jobs that are still running.
func (j *Jobs) Stop() {
	ctx := j.cron.Stop()
	<-ctx.Done()
	j.logger.Info("scheduler stopped")
}

// run executes a job once, skipping it when the previous pass has not finished.
func (j *Jobs) run(name string, job func()) {
	j.mu.Lock()
	if j.running[name] {
		j.mu.Unlock()
		j.logger.Warn("skipping a job that is still running", slog.String("job", name))
		return
	}
	j.running[name] = true
	j.mu.Unlock()

	defer func() {
		j.mu.Lock()
		delete(j.running, name)
		j.mu.Unlock()
	}()

	start := time.Now()
	job()
	j.logger.Debug("job finished", slog.String("job", name), slog.Duration("duration", time.Since(start)))
}

func (j *Jobs) scanJob() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := j.scanner.Run(ctx); err != nil {
		j.logger.Error("scheduled scan failed", slog.String("error", err.Error()))
		return
	}
	if j.cfg.IsLocal() && j.cfg.AudioCache.PrewarmEnabled && j.audio != nil {
		if targets := j.scanner.PrewarmTargets(); len(targets) > 0 {
			built := j.audio.PrewarmLibrary(ctx, j.storageService, targets)
			j.logger.Info("scheduled prewarm finished", slog.Int("variants", built))
		}
	}
}

func (j *Jobs) syncPlayCleanupJob() {
	if j.hub == nil {
		return
	}
	// Rooms left active by an earlier process can never have live sockets, so they are closed
	// before the TTL sweep decides they are old enough to delete.
	j.hub.EndStaleRooms()
	j.hub.CleanupEnded()
}

func (j *Jobs) audioCleanupJob() {
	if !j.cfg.IsLocal() {
		return
	}
	result, err := j.audio.CleanupByTTL(j.cfg.AudioCache.VariantTTL)
	if err != nil {
		j.logger.Error("audio variant cleanup failed", slog.String("error", err.Error()))
		return
	}
	if result.DeletedFiles > 0 || result.DeletedDirs > 0 || result.PrunedEntries > 0 {
		j.logger.Info("audio variant cache swept",
			slog.Int("files", result.DeletedFiles),
			slog.Int("directories", result.DeletedDirs),
			slog.Int("metadata_entries", result.PrunedEntries))
	}
}

// authCleanupJob prunes dead credentials. Expired web sessions and revoked or long-expired
// desktop sessions all accumulate rows that can never be used again.
func (j *Jobs) authCleanupJob() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cutoff := time.Now().Add(-j.cfg.Auth.ExpiredRetention)
	desktop, err := j.store.CleanupDesktopSessions(ctx, cutoff)
	if err != nil {
		j.logger.Error("desktop session cleanup failed", slog.String("error", err.Error()))
	}
	web, err := j.store.DeleteExpiredWebSessions(ctx, cutoff)
	if err != nil {
		j.logger.Error("web session cleanup failed", slog.String("error", err.Error()))
	}
	if desktop > 0 || web > 0 {
		j.logger.Info("session rows pruned",
			slog.Int64("desktop", desktop), slog.Int64("web", web), slog.String("retention", j.cfg.Auth.ExpiredRetention.String()))
	}
}
