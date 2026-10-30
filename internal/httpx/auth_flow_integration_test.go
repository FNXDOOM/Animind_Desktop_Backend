package httpx

import (
	"bytes"
	"context"
	"encoding/json"
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
