package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/cache"
)

// googleStartServer builds a server with an auth service but no database: the Google entry points
// sign state and build the provider URL, and never touch the store.
func googleStartServer(t *testing.T) *Server {
	t.Helper()
	cfg := testConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	signer, err := auth.NewSigner(cfg.Auth.TokenSigningKey)
	if err != nil {
		t.Fatalf("auth.NewSigner: %v", err)
	}
	stateCache, err := cache.Open(context.Background(), cfg.Cache, logger)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	google := auth.NewGoogle(cfg, logger)
	service := auth.NewService(nil, signer, cfg, google, stateCache, logger)
	server := New(Deps{Config: cfg, Auth: service, Signer: signer, Cache: stateCache, Logger: logger})
	server.httpServer = &http.Server{}
	return server
}

// The app-link allowlist is only a return target for the OAuth callback. It must never widen the
// origins the site itself is allowed to use, and it must never be accepted by accident.
func TestSanitizeReturnTargetAcceptsAnAppLinkOriginOnlyWhenConfigured(t *testing.T) {
	frontend := []string{"https://fnxdoom.in"}
	appLinks := []string{"https://app.example.com"}

	if got := sanitizeReturnTarget("https://app.example.com/auth", frontend); got != "" {
		t.Fatalf("app-link origin accepted without APP_LINK_ORIGINS: %q", got)
	}
	if got := sanitizeReturnTarget("https://app.example.com/auth", append(frontend, appLinks...)); got == "" {
		t.Fatal("the configured app-link origin must be accepted")
	}
}

func TestGoogleStartAnswers501WhenUnconfigured(t *testing.T) {
	server := googleStartServer(t)
	res := do(server, http.MethodGet, "/api/auth/google/start?redirect=https://app.example.com/auth", nil)
	if res.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body = %s", res.Code, res.Body)
	}
	if body := res.Body.String(); !strings.Contains(body, "oauth_not_configured") {
		t.Fatalf("body = %s, want the oauth_not_configured code", body)
	}
}

func TestGoogleStartRedirectsWithTheNonceCookie(t *testing.T) {
	t.Setenv("GOOGLE_CLIENT_ID", "test-client-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "test-client-secret")
	server := googleStartServer(t)

	res := do(server, http.MethodGet, "/api/auth/google/start?redirect=https://app.example.com/auth", nil)
	if res.Code != http.StatusFound {
		t.Fatalf("status = %d body = %s", res.Code, res.Body)
	}
	if location := res.Header().Get("Location"); !strings.HasPrefix(location, "https://accounts.google.com/") {
		t.Fatalf("Location = %q", location)
	}
	cookies := res.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want exactly the browser nonce", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != googleStateCookieName || cookie.Path != "/api/auth/google" || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v", cookie)
	}
}

func TestGoogleStartRejectsAForeignReturnTarget(t *testing.T) {
	t.Setenv("GOOGLE_CLIENT_ID", "test-client-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "test-client-secret")
	t.Setenv("APP_LINK_ORIGINS", "https://app.example.com")
	server := googleStartServer(t)

	// A foreign origin is dropped from the state, so the redirect still starts; the callback then
	// falls back to the site origin instead of sending the code to the attacker.
	res := do(server, http.MethodGet, "/api/auth/google/start?redirect=https://evil.example/steal", nil)
	if res.Code != http.StatusFound {
		t.Fatalf("status = %d", res.Code)
	}
	if strings.Contains(res.Header().Get("Location"), "evil.example") {
		t.Fatal("a foreign redirect target leaked into the provider URL")
	}
	if cookies := res.Result().Cookies(); len(cookies) != 1 {
		t.Fatalf("cookies = %v, want exactly the browser nonce", cookies)
	}
}
