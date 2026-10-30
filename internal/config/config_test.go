package config

import (
	"strings"
	"testing"
	"time"
)

// baseEnv sets the required variables so Load can succeed in a test. The default storage
// mode is s3, which needs a bucket name.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/animind")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
	t.Setenv("S3_BUCKET_NAME", "animind-library")
}

func TestLoadDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 3001 {
		t.Errorf("Port = %d, want 3001", cfg.Port)
	}
	if cfg.Storage.Mode != StorageS3 {
		t.Errorf("default StorageMode = %q, want s3", cfg.Storage.Mode)
	}
	if cfg.Auth.AccessTokenTTL != 15*time.Minute {
		t.Errorf("AccessTokenTTL = %v, want 15m", cfg.Auth.AccessTokenTTL)
	}
	if cfg.Auth.RefreshTokenTTL != 30*24*time.Hour {
		t.Errorf("RefreshTokenTTL = %v, want 720h", cfg.Auth.RefreshTokenTTL)
	}
	if cfg.Storage.TicketTTL != 4*time.Hour {
		t.Errorf("TicketTTL = %v, want 4h", cfg.Storage.TicketTTL)
	}
	if cfg.Storage.RangeChunkSize != 8<<20 {
		t.Errorf("RangeChunkSize = %d, want 8MiB", cfg.Storage.RangeChunkSize)
	}
	if cfg.Storage.NativeRangeChunkSize != 32<<20 {
		t.Errorf("native chunk = %d, want 32MiB", cfg.Storage.NativeRangeChunkSize)
	}
	if len(cfg.Scanner.VideoExtensions) != 5 {
		t.Errorf("VideoExtensions = %v, want the five documented defaults", cfg.Scanner.VideoExtensions)
	}
	if cfg.HLS.SegmentDuration != 10*time.Second {
		t.Errorf("HLS segment duration = %v, want 10s", cfg.HLS.SegmentDuration)
	}
	if cfg.SyncPlay.BufferGoal != 2*time.Minute {
		t.Errorf("buffer goal = %v, want 2m", cfg.SyncPlay.BufferGoal)
	}
	if cfg.WebhookSecret != "dev-secret" {
		t.Errorf("the dev webhook secret must stay the documented default so local setups work: %q", cfg.WebhookSecret)
	}
	if !cfg.Auth.CleanupEnabled {
		t.Error("session cleanup is on by default; a server that never prunes credentials accumulates them")
	}
}

func TestLoadRejectsMissingAndWeakSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("missing DATABASE_URL: got %v", err)
	}

	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DESKTOP_TOKEN_SIGNING_KEY") {
		t.Errorf("missing signing key: got %v", err)
	}

	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "short-key")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short signing key: got %v", err)
	}
}

func TestLoadValidatesStorageMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("STORAGE_MODE", "nfs")
	if _, err := Load(); err == nil {
		t.Fatal("an unknown STORAGE_MODE must be refused rather than silently defaulting")
	}

	t.Setenv("STORAGE_MODE", "s3")
	t.Setenv("S3_BUCKET_NAME", "")
	if _, err := Load(); err == nil {
		t.Fatal("S3 mode without a bucket cannot stream anything")
	}

	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", "/mnt/anime")
	if _, err := Load(); err != nil {
		t.Fatalf("local mode: %v", err)
	}
}

func TestLoadNormalisesEnumsAndLists(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "PRODUCTION")
	t.Setenv("WEBHOOK_SECRET", "production-webhook-secret")
	t.Setenv("VIDEO_EXTENSIONS", " MKV , mp4 ,,avi ")
	t.Setenv("FRONTEND_URL", "https://a.example, https://b.example/")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "production" || !cfg.IsProduction {
		t.Errorf("ENV = %q (production=%v), must be lower-cased", cfg.Env, cfg.IsProduction)
	}
	if got := strings.Join(cfg.Scanner.VideoExtensions, ","); got != "mkv,mp4,avi" {
		t.Errorf("VideoExtensions = %v, want trimmed and lower-cased", got)
	}
	if len(cfg.FrontendOrigins) != 2 || cfg.FrontendOrigins[0] != "https://a.example" {
		t.Errorf("FrontendOrigins = %v", cfg.FrontendOrigins)
	}
	if !cfg.Auth.SecureCookies {
		t.Error("cookies must default to Secure in production")
	}
}

func TestProductionRejectsDevelopmentWebhookSecret(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "production")
	t.Setenv("WEBHOOK_SECRET", "dev-secret")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Fatalf("production must reject the development webhook secret, got %v", err)
	}
}

func TestGetBoolAcceptsDocumentedSpellings(t *testing.T) {
	for _, truthy := range []string{"true", "TRUE", "1", "yes", "on"} {
		t.Setenv("TRUST_PROXY_HEADERS", truthy)
		if !getBool("TRUST_PROXY_HEADERS", false) {
			t.Errorf("%q should mean true", truthy)
		}
	}
	for _, falsey := range []string{"false", "0", "no", "off"} {
		t.Setenv("TRUST_PROXY_HEADERS", falsey)
		if getBool("TRUST_PROXY_HEADERS", true) {
			t.Errorf("%q should mean false", falsey)
		}
	}
	// A typo must not silently flip a security-relevant switch.
	t.Setenv("TRUST_PROXY_HEADERS", "ture")
	if getBool("TRUST_PROXY_HEADERS", false) {
		t.Error("an unrecognised value must fall back to the default, not to true")
	}
}

func TestOriginAllowed(t *testing.T) {
	baseEnv(t)
	t.Setenv("FRONTEND_URL", "https://fnxdoom.in,http://localhost:5173")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.OriginAllowed("https://fnxdoom.in") {
		t.Error("a configured origin must be allowed")
	}
	if cfg.OriginAllowed("https://evil.example") {
		t.Error("an unconfigured origin must be refused")
	}
	if !cfg.OriginAllowed("") {
		t.Error("a request with no Origin (desktop player, curl) must be allowed")
	}

	t.Setenv("FRONTEND_URL", "*")
	wildcard, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !wildcard.OriginAllowed("https://anything.example") {
		t.Error("an explicit wildcard must allow every origin")
	}
}

func TestPublicIssuerFallsBackToFirstOrigin(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicIssuer() != "http://localhost:5173" {
		t.Errorf("PublicIssuer = %q, want the first configured origin", cfg.PublicIssuer())
	}

	t.Setenv("PUBLIC_URL", "https://api.example.com/")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicIssuer() != "https://api.example.com" {
		t.Errorf("PublicIssuer = %q, want PUBLIC_URL without a trailing slash", cfg.PublicIssuer())
	}
}

func TestHLSSegmentDurationIsClamped(t *testing.T) {
	baseEnv(t)
	t.Setenv("HLS_SEGMENT_DURATION", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HLS.SegmentDuration != 2*time.Second {
		t.Errorf("segment duration = %v, must clamp to the 2s floor", cfg.HLS.SegmentDuration)
	}

	t.Setenv("HLS_SEGMENT_DURATION", "600")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HLS.SegmentDuration != 30*time.Second {
		t.Errorf("segment duration = %v, must clamp to the 30s ceiling", cfg.HLS.SegmentDuration)
	}
}

func TestParseAppLinkOrigins(t *testing.T) {
	got := parseAppLinkOrigins("https://app.example.com, https://www.example.com:8443/")
	if len(got) != 2 || got[0] != "https://app.example.com" || got[1] != "https://www.example.com:8443" {
		t.Fatalf("parseAppLinkOrigins = %v", got)
	}
	for _, bad := range []string{
		"http://app.example.com",            // app links are https only
		"https://app.example.com/auth",      // a path
		"https://app.example.com?x=1",       // a query
		"https://app.example.com#frag",      // a fragment
		"https://user:pass@app.example.com", // userinfo
		"https://*.example.com",             // wildcard
		"*",
		"app.example.com",
	} {
		if got := parseAppLinkOrigins(bad); len(got) != 0 {
			t.Errorf("parseAppLinkOrigins(%q) = %v, want none", bad, got)
		}
	}
}

func TestAppLinkOriginsComeFromTheEnvironment(t *testing.T) {
	baseEnv(t)
	t.Setenv("APP_LINK_ORIGINS", "https://app.example.com,not-an-origin")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AppLinkOrigins) != 1 || cfg.AppLinkOrigins[0] != "https://app.example.com" {
		t.Fatalf("AppLinkOrigins = %v", cfg.AppLinkOrigins)
	}
}

func TestStreamTicketTTLHasAFloor(t *testing.T) {
	baseEnv(t)
	t.Setenv("STREAM_TICKET_TTL_SECONDS", "5")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.TicketTTL != 2*time.Minute {
		t.Errorf("TicketTTL = %v, want the 120s floor", cfg.Storage.TicketTTL)
	}
}
