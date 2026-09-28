// Package config loads and validates every runtime setting from the environment.
//
// The backend is configured exclusively through environment variables so the same
// binary can run locally, in Docker, or on a VPS. Names are inherited from the
// original Node backend wherever the meaning is identical, which lets an existing
// deployment keep its .env file. Variables that only existed because of Clerk or
// Supabase-hosted auth are gone; DATABASE_URL and the GOOGLE_/AUTH_ keys replace them.
//
// Load fails fast: required keys are checked at startup rather than on first use.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// StorageMode selects where video files live.
type StorageMode string

const (
	// StorageS3 streams from an S3-compatible bucket using presigned URLs.
	StorageS3 StorageMode = "s3"
	// StorageLocal streams from a directory on the host filesystem, using
	// byte-range passthrough and on-demand HLS/ffmpeg transcoding.
	StorageLocal StorageMode = "local"
)

// Config is the fully resolved, validated set of runtime settings.
type Config struct {
	Env          string
	Port         int
	IsProduction bool
	// PublicURL is this API's externally reachable origin, used for token issuance and
	// OAuth callbacks.
	PublicURL string
	// FrontendOrigins are allowed CORS origins for the site and desktop app.
	FrontendOrigins []string
	// WebhookSecret guards POST /api/webhooks/storage.
	WebhookSecret string
	// TrustProxy enables X-Forwarded-For based client identity. Turn it on only when a
	// reverse proxy that overwrites the header sits in front of this process, otherwise a
	// client could forge it to escape rate limits.
	TrustProxy bool

	DB         DatabaseConfig
	Auth       AuthConfig
	Storage    StorageConfig
	Scanner    ScannerConfig
	Metadata   MetadataConfig
	FFmpeg     FFmpegConfig
	HLS        HLSConfig
	AudioCache AudioCacheConfig
	SyncPlay   SyncPlayConfig
}

// DatabaseConfig holds the Postgres connection settings.
type DatabaseConfig struct {
	// URL is a libpq/pgx connection string or postgres:// URL. Required.
	URL      string
	MaxConns int32
	MinConns int32
	// PingTimeout bounds the startup connectivity check.
	PingTimeout time.Duration
	// AutoMigrate applies pending migrations from the embedded FS at startup.
	AutoMigrate bool
}

// AuthConfig controls the built-in authentication system.
//
// There is no third-party identity provider: passwords are hashed with argon2id,
// web sessions are opaque bearer tokens stored hashed, and desktop access tokens
// are HMAC-signed self-contained tokens that are additionally pinned in the
// database so they can be revoked instantly.
type AuthConfig struct {
	// TokenSigningKey signs desktop access tokens. Required, high entropy.
	TokenSigningKey string
	// AccessTokenTTL is how long a desktop access token stays valid.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL is how long a desktop refresh token stays valid.
	RefreshTokenTTL time.Duration
	// WebSessionTTL is how long a site/browser session cookie or bearer session lasts.
	WebSessionTTL time.Duration
	// RememberMeTTL extends a web session when the login request sets remember_me.
	RememberMeTTL time.Duration
	// Argon2ID* tune the password hash. Defaults follow the OWASP second choice.
	Argon2Time    uint32
	Argon2Memory  uint32
	Argon2Threads uint8
	// MaxFailedLogins locks a user out for FailedLoginLockout after repeated
	// password failures on one account.
	MaxFailedLogins    int
	FailedLoginLockout time.Duration
	// GoogleClientID/Secret enable "continue with Google" on the site and desktop.
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string
	// AllowSignup restricts account creation to invites when false.
	AllowSignup bool
	// SecureCookies marks web session cookies as Secure (set true behind HTTPS).
	SecureCookies bool
	// Cleanup* prune revoked/expired desktop sessions.
	CleanupEnabled   bool
	CleanupCron      string
	ExpiredRetention time.Duration
}

// StorageConfig covers both streaming backends plus playback tickets.
type StorageConfig struct {
	Mode StorageMode
	S3   S3Config
	// LocalPath is the library root when Mode is StorageLocal.
	LocalPath string
	// PresignTTL is the lifetime of a presigned S3 GET URL.
	PresignTTL time.Duration
	// TicketTTL is the lifetime of a signed playback ticket.
	TicketTTL time.Duration
	// RangeChunkSize caps how many bytes a single range response serves to
	// browser clients. Native clients get larger chunks.
	RangeChunkSize int64
	// NativeRangeChunkSize is the chunk size served to the desktop player.
	NativeRangeChunkSize int64
}

// S3Config describes an S3-compatible endpoint (R2, Spaces, MinIO, AWS).
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	// ForcePathStyle is implied when Endpoint is set.
	ForcePathStyle bool
}

// ScannerConfig controls library indexing.
type ScannerConfig struct {
	Cron    string
	Enabled bool
	// VideoExtensions are lowercase suffixes without the leading dot.
	VideoExtensions []string
	// UseOpenRouterFallback engages the LLM parser for ambiguous filenames.
	OpenRouterFallback bool
	// PruneDeletedFiles removes episodes whose source file disappeared.
	PruneDeletedFiles bool
	// ScanOnStartup indexes the library once during boot.
	ScanOnStartup bool
}

// MetadataConfig covers AniList, MyAnimeList and the OpenRouter scanner fallback.
type MetadataConfig struct {
	AniListEnabled     bool
	AniListEndpoint    string
	AniListTimeout     time.Duration
	AniListMaxAttempts int
	MALEnabled         bool
	MALClientID        string
	MALTimeout         time.Duration
	MALMaxAttempts     int
	MetadataCacheTTL   time.Duration
	OpenRouter         struct {
		Enabled       bool
		APIKey        string
		Model         string
		SiteURL       string
		AppName       string
		Timeout       time.Duration
		MinConfidence float64
	}
}

// FFmpegConfig points at the media tooling used for subtitles, audio variants and HLS.
type FFmpegConfig struct {
	FFprobePath string
	FFmpegPath  string
	// Nice adjusts the priority of subtitle conversions on POSIX hosts.
	Nice int
	// ProcessTimeout bounds a single ffmpeg/ffprobe run.
	ProcessTimeout time.Duration
}

// HLSConfig bounds on-demand HLS sessions used by browser clients.
type HLSConfig struct {
	SegmentDuration       time.Duration
	MaxConcurrentSessions int
	// IdleTimeout destroys a session nobody has touched for this long.
	IdleTimeout time.Duration
	// ReadyTimeout bounds the wait for the first segment before a session is
	// considered failed.
	ReadyTimeout time.Duration
	// RootDir holds playlists and segments. Empty means a subdirectory of the
	// system temp dir.
	RootDir string
}

// AudioCacheConfig covers pre-generated M4A audio variants for browser playback.
type AudioCacheConfig struct {
	PrewarmEnabled             bool
	PrewarmEpisodeLimit        int
	PrewarmMaxTracksPerEpisode int
	VariantTTL                 time.Duration
	CleanupEnabled             bool
	CleanupCron                string
}

// SyncPlayConfig tunes watch-party synchronisation.
type SyncPlayConfig struct {
	Enabled bool
	// CleanupCron deletes rooms that have been ended for longer than EndedTTL.
	CleanupCron    string
	EndedTTL       time.Duration
	CleanupEnabled bool
	// ReadyTimeout is how long the host waits for members to signal ready.
	ReadyTimeout time.Duration
	// BufferGoal is the amount of media members should keep buffered.
	BufferGoal time.Duration
	// SoftSeekThreshold is the drift tolerated before the server re-syncs.
	SoftSeekThreshold time.Duration
	// SpeedSyncMaxDrift is the drift tolerated before playback rate is nudged.
	SpeedSyncMaxDrift time.Duration
	// EscapeBuffer is how far ahead of a seek members start playback.
	EscapeBuffer time.Duration
	// EscapeWait bounds the wait before a member is forced onto the new position.
	EscapeWait time.Duration
	// StallDetectWindow is how long a member's position may stay frozen before it is
	// declared stalling.
	StallDetectWindow time.Duration
	// StallGrace is how long a stalling member gets to recover before the room re-gates.
	StallGrace time.Duration
	// MaxParticipants caps a single room.
	MaxParticipants int
	// ReadLimitBytes caps one inbound socket message.
	ReadLimitBytes int64
	// PingInterval/WriteTimeout drive socket liveness.
	PingInterval time.Duration
	WriteTimeout time.Duration
	PongTimeout  time.Duration
}

// Load reads configuration from the process environment and validates it.
// key is only used for logging, keeping the error messages actionable.
func Load() (*Config, error) {
	cfg := &Config{
		Env:             strings.ToLower(strings.TrimSpace(getString("ENV", getString("NODE_ENV", "development")))),
		Port:            mustInt(getInt("PORT", 3001)),
		PublicURL:       strings.TrimSuffix(getString("PUBLIC_URL", ""), "/"),
		FrontendOrigins: splitList(getString("FRONTEND_URL", "http://localhost:5173,http://localhost:3000")),
		WebhookSecret:   getString("WEBHOOK_SECRET", "dev-secret"),
		TrustProxy:      getBool("TRUST_PROXY_HEADERS", false),
	}
	cfg.IsProduction = cfg.Env == "production"

	if err := cfg.loadDatabase(); err != nil {
		return nil, fmt.Errorf("config: database: %w", err)
	}
	if err := cfg.loadAuth(); err != nil {
		return nil, fmt.Errorf("config: auth: %w", err)
	}
	if err := cfg.loadStorage(); err != nil {
		return nil, fmt.Errorf("config: storage: %w", err)
	}
	cfg.loadScanner()
	cfg.loadMetadata()
	cfg.loadFFmpeg()
	cfg.loadHLS()
	cfg.loadAudioCache()
	cfg.loadSyncPlay()

	if cfg.WebhookSecret == "" {
		return nil, errors.New("config: WEBHOOK_SECRET must not be empty")
	}
	return cfg, nil
}

// LoadOrDie loads configuration and exits the process on error, printing the reason.
func LoadOrDie() *Config {
	cfg, err := Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "startup aborted: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

func (c *Config) loadDatabase() error {
	c.DB = DatabaseConfig{
		URL:         getString("DATABASE_URL", ""),
		MaxConns:    int32(getInt("DB_MAX_CONNS", 12)),
		MinConns:    int32(getInt("DB_MIN_CONNS", 1)),
		PingTimeout: time.Duration(getInt("DB_PING_TIMEOUT_SECONDS", 10)) * time.Second,
		AutoMigrate: getBool("DB_AUTO_MIGRATE", true),
	}
	if c.DB.URL == "" {
		return errors.New("DATABASE_URL is required (postgres://... or libpq key=value)")
	}
	return nil
}

func (c *Config) loadAuth() error {
	a := AuthConfig{
		TokenSigningKey:    getString("DESKTOP_TOKEN_SIGNING_KEY", ""),
		AccessTokenTTL:     time.Duration(getInt("DESKTOP_ACCESS_TOKEN_TTL_SECONDS", 900)) * time.Second,
		RefreshTokenTTL:    time.Duration(getInt("DESKTOP_REFRESH_TOKEN_TTL_DAYS", 30)) * 24 * time.Hour,
		WebSessionTTL:      time.Duration(getInt("WEB_SESSION_TTL_HOURS", 24)) * time.Hour,
		RememberMeTTL:      time.Duration(getInt("REMEMBER_ME_TTL_DAYS", 30)) * 24 * time.Hour,
		Argon2Time:         uint32(getInt("ARGON2_TIME", 3)),
		Argon2Memory:       uint32(getInt("ARGON2_MEMORY_KIB", 65536)),
		Argon2Threads:      uint8(getInt("ARGON2_THREADS", 4)),
		MaxFailedLogins:    getInt("AUTH_MAX_FAILED_LOGINS", 10),
		FailedLoginLockout: time.Duration(getInt("AUTH_FAILED_LOGIN_LOCKOUT_MINUTES", 15)) * time.Minute,
		GoogleClientID:     getString("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getString("GOOGLE_CLIENT_SECRET", ""),
		GoogleRedirectURL:  getString("GOOGLE_REDIRECT_URL", ""),
		AllowSignup:        getBool("ALLOW_SIGNUP", true),
		SecureCookies:      getBool("SECURE_COOKIES", c.IsProduction),
		CleanupEnabled:     getBool("DESKTOP_AUTH_CLEANUP_ENABLED", true),
		CleanupCron:        getString("DESKTOP_AUTH_CLEANUP_CRON", "30 2 * * *"),
		ExpiredRetention:   time.Duration(getInt("DESKTOP_AUTH_EXPIRED_RETENTION_DAYS", 7)) * 24 * time.Hour,
	}
	if a.TokenSigningKey == "" {
		return errors.New("DESKTOP_TOKEN_SIGNING_KEY is required (generate with: openssl rand -base64 48)")
	}
	if len(a.TokenSigningKey) < 32 {
		return errors.New("DESKTOP_TOKEN_SIGNING_KEY must be at least 32 characters")
	}
	if a.AccessTokenTTL <= 0 {
		return errors.New("DESKTOP_ACCESS_TOKEN_TTL_SECONDS must be positive")
	}
	c.Auth = a
	return nil
}

func (c *Config) loadStorage() error {
	mode := StorageMode(strings.ToLower(getString("STORAGE_MODE", "s3")))
	if mode != StorageS3 && mode != StorageLocal {
		return fmt.Errorf("STORAGE_MODE must be %q or %q, got %q", StorageS3, StorageLocal, mode)
	}
	s := StorageConfig{
		Mode: mode,
		S3: S3Config{
			Bucket:    getString("S3_BUCKET_NAME", ""),
			Region:    getString("S3_REGION", "auto"),
			Endpoint:  getString("S3_ENDPOINT", ""),
			AccessKey: getString("S3_ACCESS_KEY_ID", ""),
			SecretKey: getString("S3_SECRET_ACCESS_KEY", ""),
		},
		LocalPath:            getString("LOCAL_STORAGE_PATH", "/mnt/anime"),
		PresignTTL:           time.Duration(getInt("S3_PRESIGN_EXPIRES", 14400)) * time.Second,
		TicketTTL:            time.Duration(getInt("STREAM_TICKET_TTL_SECONDS", 14400)) * time.Second,
		RangeChunkSize:       int64(getInt("STREAM_RANGE_CHUNK_MB", 8)) * 1024 * 1024,
		NativeRangeChunkSize: int64(getInt("STREAM_NATIVE_RANGE_CHUNK_MB", 32)) * 1024 * 1024,
	}
	if s.TicketTTL < 120*time.Second {
		s.TicketTTL = 120 * time.Second
	}
	if mode == StorageS3 && s.S3.Bucket == "" {
		return errors.New("S3_BUCKET_NAME is required when STORAGE_MODE=s3")
	}
	if mode == StorageLocal && s.LocalPath == "" {
		return errors.New("LOCAL_STORAGE_PATH is required when STORAGE_MODE=local")
	}
	c.Storage = s
	return nil
}

func (c *Config) loadScanner() {
	c.Scanner = ScannerConfig{
		Cron:               getString("SCANNER_CRON", "0 */6 * * *"),
		Enabled:            getBool("SCANNER_ENABLED", true),
		VideoExtensions:    lowerList(getString("VIDEO_EXTENSIONS", "mkv,mp4,avi,webm,m4v")),
		OpenRouterFallback: getBool("SCANNER_OPENROUTER_FALLBACK", false),
		PruneDeletedFiles:  getBool("SCANNER_PRUNE_DELETED", true),
		ScanOnStartup:      getBool("SCANNER_ON_STARTUP", false),
	}
}

func (c *Config) loadMetadata() {
	c.Metadata = MetadataConfig{
		AniListEnabled:     getBool("ANILIST_ENABLED", true),
		AniListEndpoint:    getString("ANILIST_ENDPOINT", "https://graphql.anilist.co"),
		AniListTimeout:     time.Duration(getInt("ANILIST_TIMEOUT_MS", 8000)) * time.Millisecond,
		AniListMaxAttempts: getInt("ANILIST_MAX_ATTEMPTS", 3),
		MALEnabled:         getBool("MYANIMELIST_ENABLED", true),
		MALClientID:        firstNonEmpty(getString("MYANIMELIST_CLIENT_ID", ""), getString("MAL_CLIENT_ID", "")),
		MALTimeout:         time.Duration(getInt("MYANIMELIST_TIMEOUT_MS", 8000)) * time.Millisecond,
		MALMaxAttempts:     getInt("MYANIMELIST_MAX_ATTEMPTS", 2),
		MetadataCacheTTL:   time.Duration(getInt("METADATA_CACHE_TTL_HOURS", 24)) * time.Hour,
	}
	or := &c.Metadata.OpenRouter
	or.Enabled = getBool("OPENROUTER_ENABLED", false)
	or.APIKey = getString("OPENROUTER_API_KEY", "")
	or.Model = getString("OPENROUTER_MODEL", "openai/gpt-4o-mini")
	or.SiteURL = firstNonEmpty(getString("OPENROUTER_SITE_URL", ""), firstOf(c.FrontendOrigins))
	or.AppName = getString("OPENROUTER_APP_NAME", "Animind")
	or.Timeout = time.Duration(getInt("OPENROUTER_TIMEOUT_MS", 12000)) * time.Millisecond
	or.MinConfidence = getFloat("OPENROUTER_MIN_CONFIDENCE", 0.72)
}

func (c *Config) loadFFmpeg() {
	c.FFmpeg = FFmpegConfig{
		FFprobePath:    getString("FFPROBE_PATH", "ffprobe"),
		FFmpegPath:     getString("FFMPEG_PATH", "ffmpeg"),
		Nice:           getInt("FFMPEG_NICE", 19),
		ProcessTimeout: time.Duration(getInt("FFMPEG_TIMEOUT_SECONDS", 120)) * time.Second,
	}
}

func (c *Config) loadHLS() {
	seconds := getInt("HLS_SEGMENT_DURATION", 10)
	if seconds < 2 {
		seconds = 2
	}
	if seconds > 30 {
		seconds = 30
	}
	c.HLS = HLSConfig{
		SegmentDuration:       time.Duration(seconds) * time.Second,
		MaxConcurrentSessions: getInt("HLS_MAX_CONCURRENT_SESSIONS", 2),
		IdleTimeout:           time.Duration(getInt("HLS_SESSION_TIMEOUT_MINUTES", 120)) * time.Minute,
		ReadyTimeout:          time.Duration(getInt("HLS_READY_TIMEOUT_MS", 15000)) * time.Millisecond,
		RootDir:               getString("HLS_ROOT_DIR", ""),
	}
}

func (c *Config) loadAudioCache() {
	c.AudioCache = AudioCacheConfig{
		PrewarmEnabled:             getBool("POST_SCAN_AUDIO_PREWARM_ENABLED", true),
		PrewarmEpisodeLimit:        clampInt(getInt("POST_SCAN_AUDIO_PREWARM_EPISODE_LIMIT", 12), 1, 200),
		PrewarmMaxTracksPerEpisode: clampInt(getInt("POST_SCAN_AUDIO_PREWARM_MAX_TRACKS_PER_EPISODE", 1), 1, 8),
		VariantTTL:                 time.Duration(getInt("AUDIO_CACHE_VARIANT_TTL_DAYS", 14)) * 24 * time.Hour,
		CleanupEnabled:             getBool("AUDIO_CACHE_VARIANT_CLEANUP_ENABLED", true),
		CleanupCron:                getString("AUDIO_CACHE_VARIANT_CLEANUP_CRON", "15 */6 * * *"),
	}
}

func (c *Config) loadSyncPlay() {
	c.SyncPlay = SyncPlayConfig{
		Enabled:           getBool("SYNCPLAY_ENABLED", true),
		CleanupCron:       getString("SYNCPLAY_CLEANUP_CRON", "*/10 * * * *"),
		EndedTTL:          time.Duration(getInt("SYNCPLAY_ENDED_TTL_MINUTES", 60)) * time.Minute,
		CleanupEnabled:    getBool("SYNCPLAY_ENDED_CLEANUP_ENABLED", true),
		ReadyTimeout:      time.Duration(getInt("SYNCPLAY_READY_TIMEOUT_MS", 12000)) * time.Millisecond,
		BufferGoal:        time.Duration(getInt("SYNCPLAY_BUFFER_GOAL_SECONDS", 120)) * time.Second,
		SoftSeekThreshold: time.Duration(getInt("SYNCPLAY_SOFT_SEEK_THRESHOLD_MS", 200)) * time.Millisecond,
		SpeedSyncMaxDrift: time.Duration(getInt("SYNCPLAY_SPEED_SYNC_MAX_MS", 10000)) * time.Millisecond,
		EscapeBuffer:      time.Duration(getInt("SYNCPLAY_ESCAPE_BUFFER_SECONDS", 30)) * time.Second,
		EscapeWait:        time.Duration(getInt("SYNCPLAY_ESCAPE_WAIT_MS", 10000)) * time.Millisecond,
		StallDetectWindow: time.Duration(getInt("SYNCPLAY_STALL_DETECT_MS", 2000)) * time.Millisecond,
		StallGrace:        time.Duration(getInt("SYNCPLAY_STALL_GRACE_MS", 5000)) * time.Millisecond,
		MaxParticipants:   getInt("SYNCPLAY_MAX_PARTICIPANTS", 12),
		ReadLimitBytes:    int64(getInt("SYNCPLAY_MAX_MESSAGE_BYTES", 65536)),
		PingInterval:      time.Duration(getInt("SYNCPLAY_PING_INTERVAL_SECONDS", 25)) * time.Second,
		WriteTimeout:      time.Duration(getInt("SYNCPLAY_WRITE_TIMEOUT_SECONDS", 10)) * time.Second,
		PongTimeout:       time.Duration(getInt("SYNCPLAY_PONG_TIMEOUT_SECONDS", 40)) * time.Second,
	}
}

// Addr returns the listen address for the HTTP server.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

// PublicIssuer returns the externally visible origin of this API, used as the `iss`
// claim on tokens the desktop player can inspect. It prefers PUBLIC_URL and falls back
// to the first configured site origin.
func (c *Config) PublicIssuer() string {
	if c.PublicURL != "" {
		return strings.TrimSuffix(c.PublicURL, "/")
	}
	for _, origin := range c.FrontendOrigins {
		if origin != "*" {
			return strings.TrimSuffix(origin, "/")
		}
	}
	return "animind-backend"
}

// IsLocal reports whether media is served from the host filesystem.
func (c *Config) IsLocal() bool { return c.Storage.Mode == StorageLocal }

// OriginAllowed reports whether an Origin header may receive CORS headers.
func (c *Config) OriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range c.FrontendOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

func getString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func getInt(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return n
}

func getFloat(key string, fallback float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fallback
	}
	return f
}

// getBool treats only an explicit "true" (case-insensitive) as true when the
// variable is set, matching the original backend's `!== 'false'` semantics for
// defaults while rejecting typos like "ture".
func getBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func lowerList(raw string) []string {
	out := splitList(raw)
	for i := range out {
		out[i] = strings.ToLower(out[i])
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstOf(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func mustInt(v int) int { return v }
