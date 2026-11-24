package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/database"
	"animind-backend-go/internal/repository"
)

// These tests are the first ones in the repository that drive an authenticated request through the
// real store, auth service and middleware chain. They need Postgres because the behaviour under test
// is SQL plus session bookkeeping, and they skip when no disposable database is configured, exactly
// like the repository integration suite.

const testAccountPassword = "correct-horse-battery"

var (
	harnessOnce  sync.Once
	harnessSrv   *Server
	harnessStore *repository.Store
	harnessAuth  *auth.Service
	harnessErr   error
)

func sharedHarness(t *testing.T) (*Server, *repository.Store, *auth.Service) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("POSTGRES_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("set POSTGRES_TEST_DATABASE_URL to a disposable database to run the authenticated flow tests")
	}
	harnessOnce.Do(func() {
		t.Setenv("DATABASE_URL", url)
		t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
		t.Setenv("STORAGE_MODE", "local")
		t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
		t.Setenv("FRONTEND_URL", "https://fnxdoom.in,http://localhost:5173")

		cfg, err := config.Load()
		if err != nil {
			harnessErr = fmt.Errorf("config.Load: %w", err)
			return
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		slog.SetDefault(logger)

		ctx := context.Background()
		db, err := database.Open(ctx, cfg, logger)
		if err != nil {
			harnessErr = fmt.Errorf("database.Open: %w", err)
			return
		}
		if err := db.Migrate(ctx); err != nil {
			harnessErr = fmt.Errorf("migrate: %w", err)
			return
		}
		store := repository.New(db)
		stateCache, err := cache.Open(ctx, cfg.Cache, logger)
		if err != nil {
			harnessErr = fmt.Errorf("cache.Open: %w", err)
			return
		}
		signer, err := auth.NewSigner(cfg.Auth.TokenSigningKey)
		if err != nil {
			harnessErr = fmt.Errorf("auth.NewSigner: %w", err)
			return
		}
		google := auth.NewGoogle(cfg, logger)
		service := auth.NewService(store, signer, cfg, google, stateCache, logger)
		server := New(Deps{
			Config: cfg, Store: store, Auth: service, Signer: signer, Cache: stateCache, Logger: logger,
		})
		server.httpServer = &http.Server{}

		harnessSrv, harnessStore, harnessAuth = server, store, service
	})
	if harnessErr != nil {
		t.Fatalf("test harness: %v", harnessErr)
	}
	return harnessSrv, harnessStore, harnessAuth
}

type account struct {
	email string
	token string
}

// signUpAccount creates a throwaway account through the service, so a test never depends on rows a
// previous run left behind.
func signUpAccount(t *testing.T, service *auth.Service) account {
	t.Helper()
	email := fmt.Sprintf("m4-%d-%s@animind.test", time.Now().UnixNano(), strings.ToLower(t.Name()[:4]))
	result, err := service.SignUp(context.Background(), email, testAccountPassword, "")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	return account{email: email, token: result.SessionToken}
}

func (a account) do(t *testing.T, server *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+a.token)
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

type userEnvelope struct {
	User repository.PublicUser `json:"user"`
}

func (a account) currentEmail(t *testing.T, server *Server) string {
	t.Helper()
	res := a.do(t, server, http.MethodGet, "/api/auth/me", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me = %d body = %s", res.Code, res.Body)
	}
	var envelope userEnvelope
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	return envelope.User.Email
}

func errorCode(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &body)
	return body.Code
}

func TestUpdateProfileChangesEmailAndRevokesOtherSessions(t *testing.T) {
	server, _, service := sharedHarness(t)
	me := signUpAccount(t, service)
	other, err := service.SignInUser(context.Background(), mustUserID(t, server, me), "web", false)
	if err != nil {
		t.Fatalf("SignInUser: %v", err)
	}

	// Unique per run, and deliberately mixed case with surrounding spaces so normalization is proved.
	target := fmt.Sprintf("  ReNamed-%s ", me.email)
	normalizedTarget := strings.ToLower(strings.TrimSpace(target))

	res := me.do(t, server, http.MethodPatch, "/api/me", map[string]string{
		"email":            target,
		"current_password": testAccountPassword,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("PATCH /api/me = %d body = %s", res.Code, res.Body)
	}
	var envelope userEnvelope
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.User.Email != normalizedTarget {
		t.Fatalf("email = %q, want the normalized address %q", envelope.User.Email, normalizedTarget)
	}
	otherSession := account{email: me.email, token: other.SessionToken}
	if code := otherSession.do(t, server, http.MethodGet, "/api/auth/me", nil).Code; code != http.StatusUnauthorized {
		t.Fatalf("the other session answers %d, want 401", code)
	}
	if code := me.do(t, server, http.MethodGet, "/api/auth/me", nil).Code; code != http.StatusOK {
		t.Fatalf("the current session answers %d, want 200", code)
	}
	if got := me.currentEmail(t, server); got != normalizedTarget {
		t.Fatalf("current email = %q", got)
	}
}

func TestUpdateProfileEmailRequiresTheCurrentPassword(t *testing.T) {
	server, _, service := sharedHarness(t)
	me := signUpAccount(t, service)
	res := me.do(t, server, http.MethodPatch, "/api/me", map[string]string{"email": "no-password@animind.test"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	// The message must name the missing field, otherwise today's "provide username or avatar_url"
	// rejection would make this test pass for the wrong reason.
	if body := res.Body.String(); !strings.Contains(body, "current_password") {
		t.Fatalf("body = %s, want it to name current_password", body)
	}
	if got := me.currentEmail(t, server); got != me.email {
		t.Fatalf("email changed to %q without a password", got)
	}
}

func TestUpdateProfileEmailRejectsAWrongPassword(t *testing.T) {
	server, _, service := sharedHarness(t)
	me := signUpAccount(t, service)
	res := me.do(t, server, http.MethodPatch, "/api/me", map[string]string{
		"email":            "wrong-password@animind.test",
		"current_password": "not-the-password",
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if body := res.Body.String(); !strings.Contains(strings.ToLower(body), "password is incorrect") {
		t.Fatalf("body = %s, want the wrong-password message", body)
	}
	if got := me.currentEmail(t, server); got != me.email {
		t.Fatalf("email changed to %q after a wrong password", got)
	}
}

func TestUpdateProfileEmailConflict(t *testing.T) {
	server, _, service := sharedHarness(t)
	me := signUpAccount(t, service)
	other := signUpAccount(t, service)
	res := me.do(t, server, http.MethodPatch, "/api/me", map[string]string{
		"email":            other.email,
		"current_password": testAccountPassword,
	})
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.Code)
	}
}

func TestUpdateProfileEmailOnGoogleOnlyAccount(t *testing.T) {
	server, store, service := sharedHarness(t)
	email := fmt.Sprintf("google-%d@animind.test", time.Now().UnixNano())
	avatar := ""
	user, err := store.CreateUser(context.Background(), email, email, "googleuser", &avatar, nil)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	result, err := service.SignInUser(context.Background(), user.ID, "web", false)
	if err != nil {
		t.Fatalf("SignInUser: %v", err)
	}
	account := account{email: email, token: result.SessionToken}
	res := account.do(t, server, http.MethodPatch, "/api/me", map[string]string{
		"email":            "still-google@animind.test",
		"current_password": "anything",
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if code := errorCode(t, res); code != "password_not_set" {
		t.Fatalf("code = %q, want password_not_set", code)
	}
}

func TestUpdateProfileEmailAllowsKeepingYourOwnAddress(t *testing.T) {
	server, _, service := sharedHarness(t)
	me := signUpAccount(t, service)
	res := me.do(t, server, http.MethodPatch, "/api/me", map[string]string{
		"email":            me.email,
		"current_password": testAccountPassword,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", res.Code, res.Body)
	}
}

func TestPasswordChangeRevokesDesktopSessions(t *testing.T) {
	server, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	tokens, err := service.DesktopExchange(context.Background(), acct.token, "dev-revoke", "laptop")
	if err != nil {
		t.Fatalf("DesktopExchange: %v", err)
	}

	res := acct.do(t, server, http.MethodPost, "/api/auth/password/change", map[string]string{
		"current_password": testAccountPassword,
		"new_password":     "a-different-strong-password",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("password change = %d %s", res.Code, res.Body.String())
	}
	if _, err := service.DesktopRefresh(context.Background(), tokens.RefreshToken); err == nil {
		t.Fatal("desktop refresh still works after a password change")
	}
}

func TestResetPasswordRevokesDesktopSessions(t *testing.T) {
	_, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	tokens, err := service.DesktopExchange(context.Background(), acct.token, "dev-reset", "laptop")
	if err != nil {
		t.Fatalf("DesktopExchange: %v", err)
	}
	resetToken, found, err := service.RequestPasswordReset(context.Background(), acct.email)
	if err != nil || !found {
		t.Fatalf("RequestPasswordReset: found=%v err=%v", found, err)
	}
	if err := service.ResetPassword(context.Background(), resetToken, "reset-password-value"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := service.DesktopRefresh(context.Background(), tokens.RefreshToken); err == nil {
		t.Fatal("desktop refresh still works after a password reset")
	}
}

func TestBridgeTokenRejectedAfterLogout(t *testing.T) {
	_, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	principal, err := service.Authenticate(context.Background(), acct.token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	bridge, err := service.BridgeToken(context.Background(), principal)
	if err != nil {
		t.Fatalf("BridgeToken: %v", err)
	}
	if err := service.Logout(context.Background(), principal); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := service.DesktopExchange(context.Background(), bridge, "dev-bridge", "laptop"); err == nil {
		t.Fatal("bridge token minted before logout still exchanges after logout")
	}
}

func TestWebSessionRecordsClientMetadata(t *testing.T) {
	server, store, _ := sharedHarness(t)

	email := fmt.Sprintf("meta-%d@animind.test", time.Now().UnixNano())
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, testAccountPassword)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/signup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "audit-test-agent/1.0")
	req.RemoteAddr = "192.0.2.55:4321"
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup = %d %s", rec.Code, rec.Body.String())
	}

	var envelope userEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode signup: %v", err)
	}
	sessions, err := store.ListWebSessions(context.Background(), envelope.User.ID)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("ListWebSessions: %v (n=%d)", err, len(sessions))
	}
	if sessions[0].UserAgent == nil || *sessions[0].UserAgent != "audit-test-agent/1.0" {
		t.Fatalf("user agent not recorded: %v", sessions[0].UserAgent)
	}
	if sessions[0].IPAddress == nil || *sessions[0].IPAddress != "192.0.2.55" {
		t.Fatalf("ip not recorded: %v", sessions[0].IPAddress)
	}
}

func TestRefreshTokenReuseRevokesFamily(t *testing.T) {
	_, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	first, err := service.DesktopExchange(context.Background(), acct.token, "dev-reuse", "laptop")
	if err != nil {
		t.Fatalf("DesktopExchange: %v", err)
	}
	second, err := service.DesktopRefresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	if _, err := service.DesktopRefresh(context.Background(), first.RefreshToken); err == nil {
		t.Fatal("rotated-out refresh token was accepted")
	}
	if _, err := service.DesktopRefresh(context.Background(), second.RefreshToken); err == nil {
		t.Fatal("refresh family survived a reuse attempt")
	}
}

func TestGoogleSignInRefusesPasswordAccountLink(t *testing.T) {
	_, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	verified := true
	_, err := service.SignInWithGoogle(context.Background(), &auth.GoogleProfile{
		Subject:   "google-subject-" + acct.email,
		Email:     acct.email,
		EmailTrue: &verified,
	})
	if !errors.Is(err, auth.ErrAccountLinkRequired) {
		t.Fatalf("SignInWithGoogle error = %v, want ErrAccountLinkRequired", err)
	}
}

func TestLockoutDoesNotRevealAccountExistence(t *testing.T) {
	server, _, service := sharedHarness(t)
	existing := signUpAccount(t, service)
	unknown := fmt.Sprintf("missing-%d@animind.test", time.Now().UnixNano())

	attempt := func(email, remote string) (int, string) {
		body := fmt.Sprintf(`{"email":%q,"password":"wrong-password"}`, email)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		server.Router().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	var existingCode, unknownCode int
	var existingBody, unknownBody string
	for i := 0; i < 10; i++ {
		existingCode, existingBody = attempt(existing.email, "203.0.113.10:1234")
		unknownCode, unknownBody = attempt(unknown, "203.0.113.11:1234")
	}
	if existingCode != unknownCode || existingBody != unknownBody {
		t.Fatalf("account existence observable: existing=%d %q unknown=%d %q",
			existingCode, existingBody, unknownCode, unknownBody)
	}
}

func mustUserID(t *testing.T, server *Server, a account) string {
	t.Helper()
	res := a.do(t, server, http.MethodGet, "/api/auth/me", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me = %d", res.Code)
	}
	var envelope userEnvelope
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	return envelope.User.ID
}
