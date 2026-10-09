package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/database"
	"animind-backend-go/internal/repository"
)

// The credential cache is only interesting in what it must never do: answer for a credential
// the database has already rejected, or hide a change the database has already made.
//
// These run against a real Postgres so the revocations are real SQL, and against Redis as
// well when REDIS_TEST_URL is set, so the eviction is the real group index rather than a map.

const testPassword = "Corr3ct-Horse-Battery"

func serviceWithCache(t *testing.T, stateCache cache.Store) (*Service, *repository.Store) {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POSTGRES_TEST_DATABASE_URL to a disposable database to run credential cache tests")
	}

	t.Setenv("DATABASE_URL", url)
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	t.Setenv("DB_MAX_CONNS", "2")
	t.Setenv("CACHE_NAMESPACE", "animind-auth-test")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	db, err := database.Open(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	t.Cleanup(db.Close)

	if _, err := db.Pool.Exec(ctx, `
		truncate table watch_parties, watch_party_participants, user_preferences,
		         watch_history, progress, watchlist, desktop_auth_sessions,
		         animind_web_sessions, animind_auth_tokens, episodes, shows,
		         animind_user_identities, animind_users restart identity cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	store := repository.New(db)
	signer, err := NewSigner(cfg.Auth.TokenSigningKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return NewService(store, signer, cfg, NewGoogle(cfg, logger), stateCache, logger), store
}

// backendBuilders returns the stores every case runs against: the in-process one always, and
// Redis when a disposable database is configured.
func backendBuilders(t *testing.T) map[string]func(*testing.T) cache.Store {
	t.Helper()
	base := func() config.CacheConfig {
		return config.CacheConfig{
			Namespace: "animind-auth-test", SessionTTL: time.Minute, MetadataTTL: time.Hour,
			DialTimeout: 2 * time.Second, CommandTimeout: 2 * time.Second, MaxConnections: 4,
		}
	}
	builders := map[string]func(*testing.T) cache.Store{
		"local": func(t *testing.T) cache.Store {
			store, err := cache.Open(context.Background(), base(), discard())
			if err != nil {
				t.Fatalf("cache.Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	if url := os.Getenv("REDIS_TEST_URL"); url != "" {
		cfg := base()
		cfg.URL = url
		cfg.Namespace = "animind-auth-test-redis"
		builders["redis"] = func(t *testing.T) cache.Store {
			store, err := cache.Open(context.Background(), cfg, discard())
			if err != nil {
				t.Fatalf("cache.Open(redis): %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
	}
	return builders
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// warm signs a credential in and resolves it twice, so the second resolution demonstrably
// came from the cache. It fails the test when nothing was cached: otherwise every eviction
// assertion below could pass simply because the cache never held anything.
func warm(t *testing.T, service *Service, store cache.Store, email string) (*LoginResult, *Principal) {
	t.Helper()
	ctx := context.Background()

	result, err := service.SignUp(ctx, email, testPassword, "cached")
	if err != nil {
		t.Fatalf("SignUp %s: %v", email, err)
	}
	before := store.Stats()
	principal, err := service.Authenticate(ctx, result.SessionToken)
	if err != nil {
		t.Fatalf("Authenticate %s: %v", email, err)
	}
	after := store.Stats()
	if after.Writes == before.Writes {
		t.Fatalf("SignUp %s resolved without populating the cache", email)
	}
	if _, err := service.Authenticate(ctx, result.SessionToken); err != nil {
		t.Fatalf("cached Authenticate %s: %v", email, err)
	}
	if hits := store.Stats().Hits; hits == after.Hits {
		t.Fatalf("the second resolution of %s did not read the cache", email)
	}
	return result, principal
}

// mustReject asserts the credential no longer resolves — and that the store, not the cache,
// made that decision.
func mustReject(t *testing.T, service *Service, store cache.Store, token, label string) {
	t.Helper()
	before := store.Stats()
	_, err := service.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("%s: a revoked credential still authenticates (%v) — the cache is answering for it", label, err)
	}
	if store.Stats().Hits > before.Hits {
		t.Fatalf("%s: the revoked credential was still served from cache", label)
	}
}

func TestCredentialCacheServesASecondResolution(t *testing.T) {
	for name, build := range backendBuilders(t) {
		t.Run(name, func(t *testing.T) {
			store := build(t)
			service, _ := serviceWithCache(t, store)

			result, first := warm(t, service, store, "hit-"+name+"@example.com")
			ctx := context.Background()

			second, err := service.Authenticate(ctx, result.SessionToken)
			if err != nil {
				t.Fatalf("Authenticate (cached): %v", err)
			}
			if *second != *first {
				t.Fatalf("the cached principal differs from the database one:\n  %+v\n  %+v", second, first)
			}
		})
	}
}

// TestRevocationPathsEvict covers every path that ends a credential. Each gets its own
// account because the first revocation would otherwise explain away the rest.
func TestRevocationPathsEvict(t *testing.T) {
	for name, build := range backendBuilders(t) {
		t.Run(name, func(t *testing.T) {
			store := build(t)
			service, _ := serviceWithCache(t, store)
			ctx := context.Background()

			t.Run("logout", func(t *testing.T) {
				result, principal := warm(t, service, store, "logout-"+name+"@example.com")
				if err := service.Logout(ctx, principal); err != nil {
					t.Fatalf("Logout: %v", err)
				}
				mustReject(t, service, store, result.SessionToken, "logout")
			})

			t.Run("logout-everywhere", func(t *testing.T) {
				result, principal := warm(t, service, store, "everywhere-"+name+"@example.com")
				if err := service.LogoutEverywhere(ctx, principal.UserID, ""); err != nil {
					t.Fatalf("LogoutEverywhere: %v", err)
				}
				mustReject(t, service, store, result.SessionToken, "logout-everywhere")
			})

			t.Run("revoke-by-id", func(t *testing.T) {
				result, principal := warm(t, service, store, "byid-"+name+"@example.com")
				revoked, err := service.RevokeSession(ctx, principal.UserID, principal.WebSessionRowID)
				if err != nil || !revoked {
					t.Fatalf("RevokeSession: revoked=%v err=%v", revoked, err)
				}
				mustReject(t, service, store, result.SessionToken, "revoke-by-id")
			})

			t.Run("password-change", func(t *testing.T) {
				result, principal := warm(t, service, store, "change-"+name+"@example.com")
				if err := service.ChangePassword(ctx, principal.UserID, testPassword, "N3w-Correct-Horse"); err != nil {
					t.Fatalf("ChangePassword: %v", err)
				}
				mustReject(t, service, store, result.SessionToken, "password-change")
			})

			t.Run("password-reset", func(t *testing.T) {
				result, _ := warm(t, service, store, "reset-"+name+"@example.com")
				resetToken, found, err := service.RequestPasswordReset(ctx, "reset-"+name+"@example.com")
				if err != nil || !found {
					t.Fatalf("RequestPasswordReset: found=%v err=%v", found, err)
				}
				if err := service.ResetPassword(ctx, resetToken, "N3w-Correct-Horse"); err != nil {
					t.Fatalf("ResetPassword: %v", err)
				}
				mustReject(t, service, store, result.SessionToken, "password-reset")
				if _, err := service.Authenticate(ctx, resetToken); !errors.Is(err, ErrTokenInvalid) {
					t.Fatalf("the reset token was accepted as a session credential: %v", err)
				}
			})

			t.Run("deleted-account", func(t *testing.T) {
				result, principal := warm(t, service, store, "delete-"+name+"@example.com")
				if err := service.DeleteAccount(ctx, principal.UserID); err != nil {
					t.Fatalf("DeleteAccount: %v", err)
				}
				mustReject(t, service, store, result.SessionToken, "deleted-account")
			})

			t.Run("desktop-rotation", func(t *testing.T) {
				account, err := service.SignUp(ctx, "rotate-"+name+"@example.com", testPassword, "cached")
				if err != nil {
					t.Fatalf("SignUp: %v", err)
				}
				pair, err := service.issueDesktopSession(ctx, account.User.ID, "device-1", "Test Player")
				if err != nil {
					t.Fatalf("issueDesktopSession: %v", err)
				}
				principal, err := service.Authenticate(ctx, pair.AccessToken)
				if err != nil {
					t.Fatalf("Authenticate: %v", err)
				}
				if principal.Kind != "desktop" {
					t.Fatalf("Kind = %q, want desktop", principal.Kind)
				}
				if _, err := service.Authenticate(ctx, pair.AccessToken); err != nil {
					t.Fatalf("Authenticate (cached): %v", err)
				}
				if hits := store.Stats().Hits; hits == 0 {
					t.Fatal("the desktop token was never cached")
				}

				// The access token is a signed {user, session, expiry} tuple, so refreshing
				// inside the same second re-mints the identical string. Waiting makes this
				// test mean what it says: the previous credential must stop working.
				time.Sleep(1100 * time.Millisecond)
				rotated, err := service.DesktopRefresh(ctx, pair.RefreshToken)
				if err != nil {
					t.Fatalf("DesktopRefresh: %v", err)
				}
				if rotated.AccessToken == pair.AccessToken {
					t.Fatal("rotation produced the same access token; the assertion would prove nothing")
				}
				mustReject(t, service, store, pair.AccessToken, "desktop-rotation")
				if _, err := service.Authenticate(ctx, rotated.AccessToken); err != nil {
					t.Fatalf("the rotated access token does not resolve: %v", err)
				}
			})

			t.Run("desktop-revoke-by-refresh", func(t *testing.T) {
				account, err := service.SignUp(ctx, "drev-"+name+"@example.com", testPassword, "cached")
				if err != nil {
					t.Fatalf("SignUp: %v", err)
				}
				pair, err := service.issueDesktopSession(ctx, account.User.ID, "device-2", "Test Player")
				if err != nil {
					t.Fatalf("issueDesktopSession: %v", err)
				}
				if _, err := service.Authenticate(ctx, pair.AccessToken); err != nil {
					t.Fatalf("Authenticate: %v", err)
				}
				if _, err := service.Authenticate(ctx, pair.AccessToken); err != nil {
					t.Fatalf("Authenticate (cached): %v", err)
				}

				// Only the refresh token is presented: the account must still be found so its
				// cached access token can be evicted.
				revoked, err := service.DesktopRevoke(ctx, pair.RefreshToken, "")
				if err != nil || !revoked {
					t.Fatalf("DesktopRevoke: revoked=%v err=%v", revoked, err)
				}
				mustReject(t, service, store, pair.AccessToken, "desktop-revoke-by-refresh")
			})
		})
	}
}

// TestIdentityChangesAreVisibleImmediately covers the writes that do not end a credential but
// do change what it means: an admin flag and a username. Both are part of the cached
// principal, so without eviction the API would keep answering the old identity.
func TestIdentityChangesAreVisibleImmediately(t *testing.T) {
	for name, build := range backendBuilders(t) {
		t.Run(name, func(t *testing.T) {
			store := build(t)
			service, db := serviceWithCache(t, store)
			ctx := context.Background()

			result, principal := warm(t, service, store, "identity-"+name+"@example.com")
			if principal.IsAdmin {
				t.Fatal("a new account must not be an administrator")
			}

			if err := db.SetAdmin(ctx, principal.UserID, true); err != nil {
				t.Fatalf("SetAdmin: %v", err)
			}
			service.InvalidateUser(ctx, principal.UserID)

			promoted, err := service.Authenticate(ctx, result.SessionToken)
			if err != nil {
				t.Fatalf("Authenticate after promotion: %v", err)
			}
			if !promoted.IsAdmin {
				t.Fatal("the admin flag was not visible immediately: the cache answered the old identity")
			}

			if _, err := service.Authenticate(ctx, result.SessionToken); err != nil {
				t.Fatalf("Authenticate (re-warm): %v", err)
			}
			if _, err := service.UpdateProfile(ctx, principal.UserID, ptrString("renamed"), nil); err != nil {
				t.Fatalf("UpdateProfile: %v", err)
			}
			renamed, err := service.Authenticate(ctx, result.SessionToken)
			if err != nil {
				t.Fatalf("Authenticate after rename: %v", err)
			}
			if renamed.Username != "renamed" {
				t.Fatalf("username = %q, want renamed: the cache answered the old profile", renamed.Username)
			}
		})
	}
}

// TestCachedCredentialCannotOutliveItsRow is the TTL bound that makes the whole cache safe: an
// entry may never vouch for a session longer than the row itself does.
//
// The row here expires in two seconds while the configured cache TTL is a minute. If
// remember() ignored the row's expiry, this would fail four seconds later with a cached
// answer for a session the database has already let go.
func TestCachedCredentialCannotOutliveItsRow(t *testing.T) {
	base := config.CacheConfig{
		Namespace: "animind-auth-test", SessionTTL: time.Minute, MetadataTTL: time.Hour,
		DialTimeout: 2 * time.Second, CommandTimeout: 2 * time.Second, MaxConnections: 4,
	}
	store, err := cache.Open(context.Background(), base, discard())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service, db := serviceWithCache(t, store)
	ctx := context.Background()

	result, err := service.SignUp(ctx, "short-row@example.com", testPassword, "cached")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	row, err := db.ActiveWebSession(ctx, HashToken(result.SessionToken))
	if err != nil {
		t.Fatalf("ActiveWebSession: %v", err)
	}
	if err := db.TouchWebSession(ctx, row.ID, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("TouchWebSession: %v", err)
	}

	if _, err := service.Authenticate(ctx, result.SessionToken); err != nil {
		t.Fatalf("a two-second session must still resolve now: %v", err)
	}

	time.Sleep(4 * time.Second)
	if _, err := service.Authenticate(ctx, result.SessionToken); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an expired row was answered by the cache: %v", err)
	}
}

func ptrString(value string) *string { return &value }
