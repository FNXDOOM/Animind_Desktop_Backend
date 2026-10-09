// Command server runs the AniMind backend.
//
// It is the API for the desktop player and the website: library indexing, playback, user
// data, watch parties and administration. It serves no frontend assets, holds no browser
// code, and is configured entirely through environment variables.
//
// Startup order is deliberate: configuration, then the database and its migrations, then the
// services that read from it, then the socket hub, then the HTTP server. Anything that can
// fail permanently fails before the listener opens, so a half-configured server never accepts
// traffic.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/database"
	"animind-backend-go/internal/httpx"
	"animind-backend-go/internal/logging"
	"animind-backend-go/internal/media"
	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
	"animind-backend-go/internal/scheduler"
	"animind-backend-go/internal/storage"
	"animind-backend-go/internal/syncplay"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply database migrations and exit")
	showConfig := flag.Bool("config", false, "print the resolved configuration summary and exit")
	flag.Parse()

	cfg := config.LoadOrDie()
	logger := logging.Init(cfg.Env, getenv("LOG_LEVEL", "info"), getenv("LOG_FORMAT", ""))

	if *showConfig {
		summarizeConfig(cfg, logger)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg, logger)
	if err != nil {
		logger.Error("database unavailable", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	if *migrateOnly {
		if err := db.Migrate(ctx); err != nil {
			logger.Error("migration failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		logger.Info("migrations applied")
		return
	}

	store := repository.New(db)

	stateCache, err := cache.Open(ctx, cfg.Cache, logger)
	if err != nil {
		logger.Error("shared state is not usable", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		if err := stateCache.Close(); err != nil {
			logger.Warn("cache shutdown did not finish cleanly", slog.String("error", err.Error()))
		}
	}()

	signer, err := auth.NewSigner(cfg.Auth.TokenSigningKey)
	if err != nil {
		logger.Error("authentication is not usable", slog.String("error", err.Error()))
		os.Exit(1)
	}
	google := auth.NewGoogle(cfg, logger)
	authService := auth.NewService(store, signer, cfg, google, stateCache, logger)

	storageService, err := storage.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("storage is not usable", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ffmpeg := media.NewFFmpeg(cfg)
	audioCache := media.NewAudioCache(cfg, ffmpeg)

	var hlsManager *media.HLSManager
	if cfg.IsLocal() {
		hlsManager, err = media.NewHLSManager(cfg, ffmpeg, logger)
		if err != nil {
			logger.Error("HLS sessions are not usable", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}

	metadataService := metadata.New(cfg, stateCache, logger)
	openRouter := scanner.NewOpenRouter(cfg, logger)
	scannerService := scanner.New(cfg, store, storageService, metadataService, openRouter, ffmpeg, logger)

	hub := syncplay.NewHub(cfg, logger, store, authService)
	if cfg.SyncPlay.Enabled {
		logger.Info("watch parties enabled", slog.String("path", syncplay.SocketPath))
	}

	server := httpx.New(httpx.Deps{
		Config:  cfg,
		Store:   store,
		Auth:    authService,
		Signer:  signer,
		Cache:   stateCache,
		Storage: storageService,
		Media:   ffmpeg,
		Audio:   audioCache,
		HLS:     hlsManager,
		Scanner: scannerService,
		Hub:     hub,
		Logger:  logger,
	})

	jobs := scheduler.New(cfg, logger, store, storageService, scannerService, hub, audioCache)
	jobs.Start()

	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	// A first scan on boot is opt-in; when enabled it runs in the background so the API is
	// reachable immediately.
	if cfg.Scanner.Enabled && cfg.Scanner.ScanOnStartup {
		go func() {
			scanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if _, err := scannerService.Run(scanCtx); err != nil {
				logger.Error("startup scan failed", slog.String("error", err.Error()))
			}
		}()
	}

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", slog.String("error", err.Error()))
			shutdown(hub, jobs, server, hlsManager, logger)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdown(hub, jobs, server, hlsManager, logger)
}

// shutdown stops accepting work, then releases the resources that hold processes or sockets
// open, in that order.
func shutdown(hub *syncplay.Hub, jobs *scheduler.Jobs, server *httpx.Server, hls *media.HLSManager, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs.Stop()
	if hub != nil {
		hub.CloseAll()
	}
	if err := server.Shutdown(ctx); err != nil {
		logger.Warn("HTTP shutdown did not finish cleanly", slog.String("error", err.Error()))
	}
	if hls != nil {
		// Every HLS session is an ffmpeg process; killing them is what prevents orphans
		// holding segment files after an exit.
		hls.Stop()
	}
	logger.Info("shutdown complete")
}

func summarizeConfig(cfg *config.Config, logger *slog.Logger) {
	logger.Info("resolved configuration",
		slog.String("env", cfg.Env),
		slog.String("addr", cfg.Addr()),
		slog.String("issuer", cfg.PublicIssuer()),
		slog.String("storage_mode", string(cfg.Storage.Mode)),
		slog.String("local_path", cfg.Storage.LocalPath),
		slog.String("bucket", cfg.Storage.S3.Bucket),
		slog.Bool("anilist", cfg.Metadata.AniListEnabled),
		slog.Bool("myanimelist", cfg.Metadata.MALEnabled && cfg.Metadata.MALClientID != ""),
		slog.Bool("scanner_llm_fallback", cfg.Scanner.OpenRouterFallback && cfg.Metadata.OpenRouter.Enabled),
		slog.Bool("google_signin", cfg.Auth.GoogleClientID != ""),
		slog.Bool("watch_parties", cfg.SyncPlay.Enabled),
		slog.String("cache", cacheBackend(cfg)),
		slog.Duration("cache_session_ttl", cfg.Cache.SessionTTL),
		slog.Duration("cache_metadata_ttl", cfg.Cache.MetadataTTL),
		slog.String("metrics_addr", cfg.Metrics.Addr),
		slog.String("scanner_cron", cfg.Scanner.Cron),
		slog.Int("desktop_access_ttl_seconds", int(cfg.Auth.AccessTokenTTL.Seconds())),
		slog.Bool("trust_proxy_headers", cfg.TrustProxy),
	)
}

// cacheBackend names the store startup would use, without connecting to it: --config must
// report a configuration, not probe a service.
func cacheBackend(cfg *config.Config) string {
	if strings.TrimSpace(cfg.Cache.URL) == "" {
		return "in-process"
	}
	return "redis"
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
