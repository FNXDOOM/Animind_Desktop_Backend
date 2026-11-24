# Regression Tests (Proposed)

These tests are **not applied** to the repository. They are written for review alongside
`REMEDIATION_PLAN.md`; each one targets a confirmed finding and is designed to fail on the
current tree and pass after the corresponding patch.

Environment:

- pure tests: `go test ./internal/httpx -run TestClientIP`
- database-backed tests: set `POSTGRES_TEST_DATABASE_URL` to a disposable database, then
  `go test -p 1 ./...` (the existing suite skips without it and CI enforces the skip guard).
- socket tests: no external service; `httptest` server.
- AniList tests: local `httptest` upstream; no network.

---

## §1 — Password change revokes desktop sessions (F-01)

Place in `internal/httpx/auth_flow_integration_test.go` (uses the existing harness).

```go
func TestPasswordChangeRevokesDesktopSessions(t *testing.T) {
    server, _, service := sharedHarness(t)
    acct := signUpAccount(t, service)

    tokens, err := service.DesktopExchange(context.Background(), acct.token, "dev-1", "laptop")
    if err != nil {
        t.Fatalf("DesktopExchange: %v", err)
    }

    resp := acct.do(t, server, http.MethodPost, "/api/auth/password/change", map[string]string{
        "current_password": testAccountPassword,
        "new_password":     "a-different-strong-password",
    })
    if resp.Code != http.StatusOK {
        t.Fatalf("password change: %d %s", resp.Code, resp.Body.String())
    }

    if _, err := service.DesktopRefresh(context.Background(), tokens.RefreshToken); err == nil {
        t.Fatal("desktop refresh still works after a password change")
    }

    // The access token must be dead too (subject to the credential-cache TTL).
    if _, err := service.Authenticate(context.Background(), tokens.AccessToken); err == nil {
        t.Fatal("desktop access token still authenticates after a password change")
    }
}
```

**Current result:** fails at the refresh assertion (the row is untouched).

---

## §2 — Password reset revokes desktop sessions (F-01)

```go
func TestResetPasswordRevokesDesktopSessions(t *testing.T) {
    server, _, service := sharedHarness(t)
    acct := signUpAccount(t, service)

    tokens, err := service.DesktopExchange(context.Background(), acct.token, "dev-2", "desktop")
    if err != nil { t.Fatalf("DesktopExchange: %v", err) }

    resetToken, found, err := service.RequestPasswordReset(context.Background(), acct.email)
    if err != nil || !found { t.Fatalf("RequestPasswordReset: found=%v err=%v", found, err) }

    if err := service.ResetPassword(context.Background(), resetToken, "reset-password-value"); err != nil {
        t.Fatalf("ResetPassword: %v", err)
    }
    if _, err := service.DesktopRefresh(context.Background(), tokens.RefreshToken); err == nil {
        t.Fatal("desktop refresh still works after a password reset")
    }
    _ = server
}
```

---

## §3 — Email change revokes desktop sessions (F-01)

The existing `TestUpdateProfileChangesEmailAndRevokesOtherSessions` covers web sessions. Extend it
with a desktop pair created before the change and assert `DesktopRefresh` fails afterwards.

---

## §4 — Session cookie attributes (F-15, informational)

```go
func TestSessionCookieIsHostScoped(t *testing.T) {
    rec := httptest.NewRecorder()
    srv := testServer(t)
    srv.setSessionCookie(rec, "as_test", time.Now().Add(time.Hour), false)

    cookies := rec.Result().Cookies()
    if len(cookies) != 1 { t.Fatalf("got %d cookies", len(cookies)) }
    c := cookies[0]
    if c.Domain != "" { t.Errorf("cookie must not set Domain, got %q", c.Domain) }
    if !c.HttpOnly { t.Error("cookie must be HttpOnly") }
    if !strings.HasPrefix(c.Name, "__Host-") {
        t.Errorf("cookie should use the __Host- prefix, got %q", c.Name)
    }
}
```

Current tree: fails the prefix assertion (expected; informational).

---

## §5 — `ClientIP` handles IPv6 (F-07)

```go
func TestClientIPHandlesIPv6(t *testing.T) {
    cases := []struct{ remote, want string }{
        {"192.0.2.10:5555", "192.0.2.10"},
        {"[2001:db8::1]:5555", "2001:db8::1"},
        {"[::1]:5555", "::1"},
    }
    for _, tc := range cases {
        r := httptest.NewRequest(http.MethodGet, "/", nil)
        r.RemoteAddr = tc.remote
        if got := ClientIP(r, false); got != tc.want {
            t.Errorf("ClientIP(%q) = %q, want %q", tc.remote, got, tc.want)
        }
    }
}
```

**Current result:** returns `[2001` / `[` for the IPv6 rows.

---

## §6 — Socket handshake is rate-limited (F-02)

```go
func TestSocketHandshakeIsRateLimited(t *testing.T) {
    srv := socketServer(t) // existing helper in socket_middleware_test.go
    base := srv.URL + syncplay.SocketPath + "?EIO=4&transport=polling"

    limited := false
    for i := 0; i < 40; i++ {
        resp, err := http.Get(base)
        if err != nil { t.Fatalf("request %d: %v", i, err) }
        _ = resp.Body.Close()
        if resp.StatusCode == http.StatusTooManyRequests {
            limited = true
            break
        }
    }
    if !limited {
        t.Fatal("40 polling handshakes produced no 429; the socket route is unthrottled")
    }
}
```

**Current result:** all 40 return 200 and create peers.

---

## §7 — Unauthenticated socket is closed (F-02)

```go
func TestPreAuthSocketIsClosedWithoutConnect(t *testing.T) {
    srv := socketServer(t)
    url := "ws" + strings.TrimPrefix(srv.URL, "http") + syncplay.SocketPath + "?EIO=4&transport=websocket"
    conn, _, err := websocket.DefaultDialer.Dial(url, nil)
    if err != nil { t.Fatalf("dial: %v", err) }
    defer conn.Close()

    _ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
    for {
        if _, _, err := conn.ReadMessage(); err != nil {
            return // closed by the server, as required
        }
    }
}
```

**Current result:** the client stays connected past 15 s (server pings keep it alive).

---

## §8 — AniList cache refuses oversized documents (F-03)

Extend `internal/httpx/caching_test.go`. Use a local upstream that returns a large, valid GraphQL
document and assert it is not cached.

```go
func TestAniListCacheRefusesOversizeDocument(t *testing.T) {
    big := `{"data":{"Media":{"description":"` + strings.Repeat("x", 600<<10) + `"}}}`
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        _, _ = io.WriteString(w, big)
    }))
    defer upstream.Close()

    srv := testServerWith(t, func(d *Deps) { d.Config.Metadata.AniListEndpoint = upstream.URL })
    body := `{"query":"{ Media(id: 1) { description } }"}`

    for i := 0; i < 2; i++ {
        req := httptest.NewRequest(http.MethodPost, "/api/anilist", strings.NewReader(body))
        rec := httptest.NewRecorder()
        srv.Router().ServeHTTP(rec, req)
        if got := rec.Header().Get("X-Cache"); got != "MISS" {
            t.Fatalf("request %d: X-Cache=%q, oversized document was cached", i+1, got)
        }
    }
}
```

**Current result:** the second request is a HIT.

---

## §9 — Google link requires proof of the existing account (F-04)

```go
func TestGoogleLinkRequiresExistingAccountProof(t *testing.T) {
    _, _, service := sharedHarness(t)
    acct := signUpAccount(t, service) // attacker owns this password account

    verified := true
    profile := &auth.GoogleProfile{
        Subject: "google-subject-" + acct.email,
        Email:   acct.email,
        EmailTrue: &verified,
        Name:    "Victim",
    }
    // The desired behaviour: refuse to attach a provider to an existing password
    // account until the owner proves control of that account.
    if _, err := service.SignInWithGoogle(context.Background(), profile); err == nil {
        t.Fatal("Google identity was silently linked to a pre-existing password account")
    }
}
```

**Current result:** a session is returned (the link succeeds).

---

## §10 — Unverified signup cannot authenticate (F-04)

```go
func TestUnverifiedSignupCannotBeUsed(t *testing.T) {
    server, _, service := sharedHarness(t)
    acct := signUpAccount(t, service) // after the fix: returns a pending account, no token

    if acct.token == "" {
        return // signup correctly withheld the session
    }
    resp := acct.do(t, server, http.MethodGet, "/api/auth/me", nil)
    if resp.Code == http.StatusOK {
        t.Fatal("unverified account can call an authenticated endpoint")
    }
}
```

**Current result:** signup returns a usable session, so `/api/auth/me` is 200.

---

## §11 — Config rejects placeholder secrets (F-05)

```go
func TestConfigRejectsPlaceholderSecrets(t *testing.T) {
    t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
    t.Setenv("ENV", "production")
    t.Setenv("SECURE_COOKIES", "true")
    t.Setenv("FRONTEND_URL", "https://example.test")
    t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "replace-with-a-long-random-secret")
    t.Setenv("WEBHOOK_SECRET", "change-me")

    if _, err := config.Load(); err == nil {
        t.Fatal("config accepted documented placeholder secrets")
    }
}
```

**Current result:** loads successfully (33-char key passes the length check; `change-me` is not
the rejected literal).

---

## §12 — Config fails closed without an environment (F-05)

```go
func TestConfigFailsClosedWithoutEnvironment(t *testing.T) {
    t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
    t.Setenv("PUBLIC_URL", "https://animind.example.test")
    t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
    t.Setenv("WEBHOOK_SECRET", "a-real-webhook-secret")
    // ENV deliberately unset
    if _, err := config.Load(); err == nil {
        t.Fatal("config defaulted to development with a public URL")
    }
}
```

**Current result:** loads with `ENV=development`.

---

## §13 — Lockout does not reveal account existence (F-06)

```go
func TestLockoutDoesNotRevealAccountExistence(t *testing.T) {
    server, _, service := sharedHarness(t)
    existing := signUpAccount(t, service)
    unknown := "definitely-not-registered@animind.test"

    type outcome struct{ code int; body string }
    attempt := func(email string) outcome {
        body := fmt.Sprintf(`{"email":%q,"password":"wrong-password"}`, email)
        req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        rec := httptest.NewRecorder()
        server.Router().ServeHTTP(rec, req)
        return outcome{rec.Code, rec.Body.String()}
    }

    var existingOutcome outcome
    for i := 0; i < 12; i++ { existingOutcome = attempt(existing.email) }
    var unknownOutcome outcome
    for i := 0; i < 12; i++ { unknownOutcome = attempt(unknown) }

    if existingOutcome.code != unknownOutcome.code || existingOutcome.body != unknownOutcome.body {
        t.Fatalf("account existence is observable: existing=%d %q unknown=%d %q",
            existingOutcome.code, existingOutcome.body, unknownOutcome.code, unknownOutcome.body)
    }
}
```

**Current result:** existing account returns 423 on the 10th attempt; unknown stays 401.

---

## §14 — HLS session cannot be controlled by another user (F-08)

Requires the owner-aware patch and a real source file (skip if no ffmpeg).

```go
func TestHLSSessionCannotBeControlledByAnotherUser(t *testing.T) {
    t.Skip("requires ffmpeg and a small local sample; enable in the media integration suite")
    // manager := NewHLSManager(cfg, ffmpeg, logger)
    // id, _, err := manager.Create(ctx, sample, "episode-1", 0, 0, "user-A")
    // if err := manager.Seek(ctx, id, 5, "user-B"); !errors.Is(err, ErrForbidden) { ... }
    // manager.Destroy(ctx, id, "user-B") // must not remove user-A's session
}
```

**Current result:** no user parameter exists; any authenticated caller with the id can seek or
destroy.

---

## §15 — Room joins are throttled (F-09)

```go
func TestJoinRoomIsRateLimited(t *testing.T) {
    // Build a hub and an authenticated peer using the existing syncplay test helpers,
    // then emit joinRoom with a random code more than the configured limit allows.
    // Assert that the server starts returning a denial (or closes the socket) and that
    // a legitimate join from another account still works.
    t.Skip("wire to the existing syncplay functional harness once the limiter exists")
}
```

Documented as a functional test rather than a unit test because the limiter will live in the hub.
**Current result:** no limiter, so the test cannot be written without the patch.

---

## §16 — Bridge token rejected after logout (F-10)

```go
func TestBridgeTokenRejectedAfterLogout(t *testing.T) {
    _, _, service := sharedHarness(t)
    acct := signUpAccount(t, service)

    bearer, err := service.Authenticate(context.Background(), acct.token)
    if err != nil { t.Fatalf("Authenticate: %v", err) }
    bridge, err := service.BridgeToken(context.Background(), bearer)
    if err != nil { t.Fatalf("BridgeToken: %v", err) }

    if err := service.Logout(context.Background(), bearer); err != nil { t.Fatalf("Logout: %v", err) }

    if _, err := service.DesktopExchange(context.Background(), bridge, "dev-3", "laptop"); err == nil {
        t.Fatal("bridge token minted before logout still exchanges after logout")
    }
}
```

**Current result:** exchange succeeds (signature/aud/exp only).

---

## §17 — Refresh-token reuse revokes the family (F-11)

```go
func TestRefreshReuseRevokesFamily(t *testing.T) {
    _, _, service := sharedHarness(t)
    acct := signUpAccount(t, service)

    first, err := service.DesktopExchange(context.Background(), acct.token, "dev-4", "laptop")
    if err != nil { t.Fatalf("DesktopExchange: %v", err) }

    second, err := service.DesktopRefresh(context.Background(), first.RefreshToken)
    if err != nil { t.Fatalf("first refresh: %v", err) }

    // Replaying the rotated-out token is the theft signal.
    if _, err := service.DesktopRefresh(context.Background(), first.RefreshToken); err == nil {
        t.Fatal("rotated-out refresh token was accepted")
    }
    // The family must be revoked, so the attacker's newest token dies too.
    if _, err := service.DesktopRefresh(context.Background(), second.RefreshToken); err == nil {
        t.Fatal("refresh family survived a reuse attempt")
    }
}
```

**Current result:** the first replay is rejected, but `second` still works (no family state).

---

## §18 — Wildcard CORS never allows credentials (F-12)

```go
func TestCORSWithWildcardNeverAllowsCredentials(t *testing.T) {
    srv := testServerWith(t, func(d *Deps) { d.Config.FrontendOrigins = []string{"*"} })
    req := httptest.NewRequest(http.MethodGet, "/api/shows", nil)
    req.Header.Set("Origin", "https://evil.example")
    rec := httptest.NewRecorder()
    srv.Router().ServeHTTP(rec, req)

    if rec.Header().Get("Access-Control-Allow-Credentials") == "true" {
        t.Fatal("wildcard origin was combined with allow-credentials")
    }
    if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" && got != "" {
        t.Fatalf("unexpected allow-origin %q", got)
    }
}
```

**Current result:** reflects `https://evil.example` and sets credentials.

---

## §19 — Concurrent download creates respect the quota (F-16)

Database-backed; the manager test already has a fake store, but the quota race needs the real
transaction boundary or a serialising lock. Use the repository integration harness.

```go
func TestConcurrentDownloadCreateRespectsQuota(t *testing.T) {
    // cfg.MaxActivePerUser = 1
    // start N goroutines calling Manager.Create with distinct info hashes
    // wait; count active rows for the user
    // assert count <= 1 (or <= MaxActivePerUser)
    // and assert that a duplicate create returns ErrDuplicate / 409, not a 500
    t.Skip("requires the Postgres integration harness; enable with POSTGRES_TEST_DATABASE_URL")
}
```

**Current result:** N-1 extra jobs can be created; a duplicate race surfaces a wrapped unique
violation and a 500.

---

## 20 — Session records client metadata (F-22)

```go
func TestWebSessionRecordsClientMetadata(t *testing.T) {
    server, store, _ := sharedHarness(t)

    body := fmt.Sprintf(`{"email":%q,"password":%q}`,
        fmt.Sprintf("meta-%d@animind.test", time.Now().UnixNano()), testAccountPassword)
    req := httptest.NewRequest(http.MethodPost, "/api/auth/signup", strings.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("User-Agent", "audit-test-agent/1.0")
    req.RemoteAddr = "192.0.2.55:4321"
    rec := httptest.NewRecorder()
    server.Router().ServeHTTP(rec, req)
    if rec.Code != http.StatusCreated {
        t.Fatalf("signup: %d %s", rec.Code, rec.Body.String())
    }

    var payload struct {
        User struct{ ID string `json:"id"` } `json:"user"`
    }
    if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
        t.Fatalf("decode signup: %v", err)
    }
    sessions, err := store.ListWebSessions(context.Background(), payload.User.ID)
    if err != nil || len(sessions) == 0 {
        t.Fatalf("ListWebSessions: %v (n=%d)", err, len(sessions))
    }
    if sessions[0].UserAgent != "audit-test-agent/1.0" {
        t.Fatalf("user agent not recorded: %q", sessions[0].UserAgent)
    }
    if sessions[0].IPAddress != "192.0.2.55" {
        t.Fatalf("ip not recorded: %q", sessions[0].IPAddress)
    }
}
```

**Current result:** both fields are empty because the signup/login handlers never call
`auth.WithClient`.

---

## 21. Existing tests that already cover verified controls

These should remain green and are the regression net for the "no finding" conclusions:

| Control | Existing test |
|---|---|
| argon2id verify + parameter upgrade | `TestHashAndVerifyPassword`, `TestVerifyPasswordDetectsParameterUpgrade` |
| weak password rejection | `TestHashPasswordRejectsWeakInput` |
| Google verified-email requirement | `TestValidateGoogleProfileRequiresVerifiedEmail` |
| OAuth state single-use + browser binding | `TestGoogleStateIsBrowserBoundAndSingleUse` |
| JWT alg/audience/issuer/tamper | `TestBridgeTokenRequiresHS256AndAudience`, `TestBridgeTokenIssuerMustMatchDeployment`, `TestDesktopAccessTokenRejectsTampering` |
| stream ticket binding | `TestStreamTicketBindsEpisodeClientModeAndTrack` |
| login throttle scoping | `TestLoginFailureThrottleIsScopedToSource` |
| token entropy/size | `TestGeneratedTokensAreRandomAndWellSized` |
| route auth coverage | `TestProtectedRoutesRejectAnonymous` |
| CORS allowlist | `TestCORSPreflightAndAllowedOrigin` |
| security headers | `TestSecurityHeadersAreSet` |
| rate-limiter independence | `TestRateLimitersAreIndependentPerRoute` |
| same-site/redirect allowlist | `TestSanitizeReturnTargetRejectsForeignHosts` |
| GraphQL query restriction | `TestIsReadOnlyGraphQL` |
| AniList cache miss on bad response | `TestAniListProxyDoesNotCacheABadResponse` |
| socket pre-auth event refusal | `TestWebSocketApplicationEventsRequireAuthentication`, `TestPollingApplicationEventsRequireAuthentication` |
| download ownership/state | `TestManagerActionsEnforceOwnership`, `TestPauseAndResumeGuardTheTable` |
| import no-overwrite / symlink defense | `internal/downloads/importer_test.go` cases |

## 22. Suggested CI addition

```yaml
- name: Security checks
  run: |
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...
    # optionally:
    # go run github.com/securego/gosec/v2/cmd/gosec@latest -quiet ./...
```

Pin the tool versions and fail on reachable findings. Run the database suite with
`POSTGRES_TEST_DATABASE_URL` exactly as the existing test job does.
