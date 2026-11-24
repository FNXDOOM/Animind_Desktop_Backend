# Remediation Plan

**Status (2026-11-25):** the P0/P1/P2 code fixes in this plan have been implemented and verified
locally. See `REMEDIATION_STATUS.md` for the per-finding result and the remaining open items.
This document is retained as the remediation design record; nothing was committed or deployed.

Priority scale:

| Priority | Meaning | Target |
|---|---|---|
| **P0** | Restores a security control that currently fails its documented purpose, or removes an unauthenticated availability kill-switch | before the next public deployment |
| **P1** | High-value hardening with a clear regression test | within one release |
| **P2** | Defence-in-depth / correctness | within one quarter |
| **P3** | Hygiene, documentation, supply chain | ongoing |

---

## Phase 0 — P0 (do first)

### P0-1 — Revoke desktop sessions on every credential-reset event (F-01)

**Files:** `internal/repository/desktopsessions.go`, `internal/auth/service.go`

```go
// internal/repository/desktopsessions.go
func (s *Store) RevokeAllDesktopSessions(ctx context.Context, userID string) error {
    _, err := s.pool.Exec(ctx, `
        update desktop_auth_sessions
        set revoked_at = now(), updated_at = now()
        where user_id = $1::uuid and revoked_at is null`, userID)
    return err
}
```

Add the call after `RevokeAllWebSessions` in `ChangePassword`, `ResetPassword` and `ChangeEmail`:

```go
if err := s.store.RevokeAllWebSessions(ctx, userID, keepSessionID); err != nil {
    return err
}
if err := s.store.RevokeAllDesktopSessions(ctx, userID); err != nil {
    return err
}
s.InvalidateUser(ctx, userID)
```

**Acceptance:** after a password change, `POST /api/auth/desktop/refresh` with the pre-change
refresh token returns 401; a desktop access token stops working within the credential-cache TTL
(≤60 s) and immediately after cache invalidation.

**Effort:** ~1 hour + test.

---

### P0-2 — Bound the unauthenticated socket surface (F-02)

**Files:** `internal/httpx/ratelimit.go`, `internal/httpx/server.go`,
`internal/syncplay/hub.go`, `internal/syncplay/protocol.go`

1. Add a `SocketHandshake` limiter (e.g. 30/min/IP) and register the hub behind it. Because
   `hub.Register` takes a `chi.Router`, pass a scoped router:

```go
// server.go
socketGroup := chi.NewRouter()
socketGroup.Use(s.rate(s.limiter.SocketHandshake))
s.hub.Register(socketGroup)
r.Mount("/", socketGroup) // or mount on the exact path group
```

(If mounting is awkward, give `Hub.Register` a `func(http.Handler) http.Handler` middleware
parameter instead; the point is that the limiter runs before `serveSocket`.)

2. Add a global pre-auth peer cap in `Hub`:

```go
const maxPeers = 1000
const maxPreAuthPeers = 200
const authDeadline = 10 * time.Second
```

In `websocketHandler`, after `newPeer`, start a timer that closes the peer if
`!p.authenticated.Load()` after `authDeadline`. In `pollingHandler` GET, refuse with 503 when the
cap is reached. Track `preauth` count separately under `h.mu`.

3. Add nginx `limit_conn`/`limit_req` on `/api/socket.io` as a second layer:

```nginx
limit_req_zone $binary_remote_addr zone=socket:10m rate=30r/m;
limit_conn_zone $binary_remote_addr zone=socketconn:10m;
location = /api/socket.io {
    limit_req zone=socket burst=10 nodelay;
    limit_conn socketconn 5;
    ...
}
```

**Acceptance:** the 31st handshake in a minute from one IP returns 429; 201 pre-auth peers are
refused; an idle unauthenticated WebSocket is closed at 10 s.

**Effort:** 2–4 hours + tests.

---

### P0-3 — Bound the AniList cache by bytes (F-03)

**Files:** `internal/httpx/handler_library.go`, `internal/cache/local.go`,
`internal/cache/cache.go`

```go
// handler_library.go
const maxCachedAniListBytes = 256 << 10
if len(upstream) > maxCachedAniListBytes {
    // relay it, but do not cache it
} else if isUsableGraphQLDocument(upstream) {
    s.rememberAniList(...)
}
```

```go
// cache/local.go
type localStore struct {
    ...
    bytes int
    maxBytes int // e.g. 64 << 20
}
```

Subtract on delete/evict, add on set, and evict oldest-by-expiry until both `maxLocalEntries` and
`maxBytes` are satisfied. For Redis, document `maxmemory` + `volatile-lru` as a requirement and
consider `maxmemory-policy allkeys-lru` if the cache shares a Redis with nothing else.

**Acceptance:** a 1 MiB AniList response is relayed but not cached; the local store never exceeds
its byte budget under a synthetic loop.

**Effort:** 2–3 hours.

---

### P0-4 — Make the safe production configuration the default (F-05)

**Files:** `internal/config/config.go`, `.env.example`, `docker-compose.yml` (or split into
`docker-compose.dev.yml`)

1. Replace the implicit default:

```go
env := strings.ToLower(strings.TrimSpace(os.Getenv("ENV")))
if env == "" {
    // fail closed unless the deployment is clearly local
    if IsLoopbackURL(getString("PUBLIC_URL", "")) {
        env = "development"
    } else {
        return nil, errors.New("config: ENV must be set (production|development)")
    }
}
```

2. Default `SecureCookies` to true. Require `INSECURE_COOKIES=true` for local HTTP.
3. Reject placeholder secrets in **every** environment:

```go
func rejectPlaceholder(name, value string) error {
    lowered := strings.ToLower(value)
    for _, marker := range []string{
        "change-me", "replace-with", "local-development", "dev-secret",
        "please-replace", "your-secret", "password",
    } {
        if strings.Contains(lowered, marker) {
            return fmt.Errorf("config: %s still contains a placeholder value", name)
        }
    }
    return nil
}
```

Apply to `DESKTOP_TOKEN_SIGNING_KEY`, `WEBHOOK_SECRET`, `S3_SECRET_ACCESS_KEY`,
`GOOGLE_CLIENT_SECRET`, `OPENROUTER_API_KEY`.
4. Reject `FRONTEND_URL=*` in every environment; add an explicit `ALLOW_ANY_ORIGIN=true` only if
   someone truly needs it, and never combine it with credentials.
5. Gate `safeDetail` on `DEBUG_ERRORS=true`, default false.
6. Move the current compose file to `docker-compose.dev.yml`; make the default compose file bind
   nginx to `127.0.0.1` and require secrets from the environment (`${VAR:?must be set}`).

**Acceptance:** `ENV` unset + public `PUBLIC_URL` → startup error; placeholder key → startup
error; `FRONTEND_URL=*` → startup error; unit tests cover each.

**Effort:** 3–5 hours.

---

## Phase 1 — P1

### P1-1 — Verify email before the account is usable (F-04)

**Files:** migrations (new `email_verified_at` column), `internal/auth/service.go`,
`internal/httpx/handler_auth.go`, plus a mail integration or an out-of-band admin flow.

- Signup inserts `email_verified_at = null`, sends a token, and returns 202 without a session.
- `POST /api/auth/verify-email` consumes the token and sets the timestamp.
- `Login` refuses an unverified account (generic error).
- `SignInWithGoogle`:
  - if the matching account is **verified**, require a confirmation step before linking;
  - if unverified, do not link; complete verification first;
  - if no account, create an active account (Google already verified the address).
- Optionally allow a one-time grace login for local single-user setups via config.

**Acceptance:** an attacker who registers a victim's address cannot log in or block the victim's
Google sign-in; the victim's Google login creates/links only after proving the address.

**Effort:** 1–2 days (mostly mail/UX).

---

### P1-2 — Uniform authentication responses (F-06)

**Files:** `internal/auth/service.go`, `internal/httpx/handler_auth.go`

- Return `401` with the same body for lockout and invalid credentials; log `locked` separately.
- Signup: return `202` with a generic message and an email flow (preferred), or keep 409 and
  accept the enumeration trade-off with a stricter limiter.
- Do not echo `ErrEmailTaken` text in the body.

**Acceptance:** 10 failed logins on an existing account and 10 on an unknown account produce the
same status/body (timing may differ only within noise).

**Effort:** 1–2 hours.

---

### P1-3 — Fix `ClientIP` for IPv6 (F-07)

**File:** `internal/httpx/responses.go`

```go
import "net"

func ClientIP(r *http.Request, trustProxy bool) string {
    if trustProxy {
        if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
            first := strings.TrimSpace(strings.Split(xf, ",")[0])
            if net.ParseIP(first) != nil { return first }
        }
        if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
            return real
        }
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil { return r.RemoteAddr }
    return host
}
```

**Acceptance:** `[2001:db8::1]:1234 → 2001:db8::1`; login/signup works over IPv6; rate-limit keys
are per address.

**Effort:** 30 minutes.

---

### P1-4 — Bind HLS sessions to their owner (F-08)

**Files:** `internal/media/hls.go`, `internal/httpx/handler_media.go`

- Add `UserID string` to `HLSSession`; `Create(ctx, sourcePath, episodeID, audioTrack, startTime,
  userID)`.
- `Seek`, `Destroy`, `Heartbeat` take `userID` and compare.
- `Stats()` still exists for admins; the per-session heartbeat returns only that session.

**Acceptance:** user B cannot seek/destroy user A's session even with the id; heartbeat does not
list other sessions.

**Effort:** 2–3 hours.

---

### P1-5 — Strengthen room codes and throttle joins (F-09)

**Files:** `internal/syncplay/hub.go`, `internal/syncplay/room.go`,
`internal/httpx/handler_library.go`

- Increase `roomCodeLength` to 7 or 8.
- Add a per-user failed-join limiter (e.g. 10/min, 5-minute lockout) and a per-IP limiter.
- Remove `HostUserID` from the lookup response or return a display name only.
- Optionally require host approval for joins.

**Acceptance:** 11 failed joins return a refusal/backoff; the code space is ≥ 32^7; the lookup no
longer exposes a user id.

**Effort:** 1–2 hours.

---

### P1-6 — Close the refresh-token family on replay (F-11) and bind bridge tokens (F-10)

**Files:** `internal/repository/desktopsessions.go` (new columns), `internal/auth/service.go`

- Add `family_id uuid`, `rotated_from_hash text`, `absolute_expires_at timestamptz`.
- On refresh, if the presented hash matches `rotated_from_hash`, revoke the whole family (all
  rows sharing `family_id`) and return 401.
- Cap `absolute_expires_at` at e.g. 90 days; do not extend it on rotation.
- Bridge tokens: add `WebSessionRowID` to `BridgeClaims`; in `DesktopExchange`, load the web
  session row and require `revoked_at is null and expires_at > now()` before issuing desktop
  tokens.
- Log/alert on reuse.

**Acceptance:** replaying a rotated token revokes the family; a bridge token minted before logout
returns 401 after logout.

**Effort:** 4–6 hours.

---

### P1-7 — Fix CORS credentials/wildcard (F-12)

**File:** `internal/httpx/middleware.go`

```go
if origin == "*" {
    h.Set("Access-Control-Allow-Origin", "*")
    // do NOT set Allow-Credentials
} else if s.cfg.OriginAllowed(origin) {
    h.Set("Access-Control-Allow-Origin", origin)
    h.Set("Access-Control-Allow-Credentials", "true")
    h.Add("Vary", "Origin")
}
```

Reject `*` in `validateProductionSecurity` for all environments.

**Acceptance:** configured wildcard yields `*` without credentials; a reflected origin still
requires an exact allowlist match.

**Effort:** 1 hour.

---

### P1-8 — Upgrade reachable dependencies (F-13)

```bash
go get github.com/gorilla/websocket@latest
go get go.opentelemetry.io/otel@v1.47.0 \
      go.opentelemetry.io/otel/trace@v1.47.0 \
      go.opentelemetry.io/otel/metric@v1.47.0
go mod tidy && go test ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Add `govulncheck` to CI.

**Acceptance:** no reachable vulnerabilities in `govulncheck`; race tests pass.

**Effort:** 1 hour.

---

## Phase 2 — P2

### P2-1 — Make download quota atomic (F-16)

Use a transaction + advisory lock around the count/insert, or a single conditional insert. Map
`23505` to `DuplicateJobError`. Add a concurrent test.

### P2-2 — Single-row entitlement lookup (F-17)

Add `Store.CanDownload(ctx, userID)` and use it from `storeEntitlements`. Remove the page loop.

### P2-3 — Cookie hardening (F-15)

Rename the cookie to `__Host-animind_session` when `SecureCookies` is true; keep a read fallback
for one release. Do not set `Domain`.

### P2-4 — Error detail and log hygiene (F-19, F-20)

Gate details on `DEBUG_ERRORS`; log user id instead of email in the recovery path; drop the token
hash; configure an nginx access-log format that omits query strings for `/api/` or masks `st`,
`code`, `state`.

### P2-5 — CI and image supply chain (F-18)

SHA-pin actions; add gosec/Semgrep/gitleaks/SBOM; move off `alpine:3.20`; digest-pin images;
add `govulncheck`.

### P2-6 — Runtime hardening

Read-only root, `cap_drop: ALL`, `no-new-privileges`, CPU/memory/PID limits, log rotation, and
nginx `limit_req`/`limit_conn`. These directly reduce the impact of F-02/F-03.

### P2-7 — HLS seek debounce and session-cap atomicity

Reject a second seek within a short window per session; perform the cap check and insert under
one lock.

### P2-8 — Record session client metadata (F-22)

Attach `auth.WithClient` in `handleSignup`, `handleLogin`, `handleGoogleCallback` and
`handleConsumeCode`, or change `SignInUser` to take a `Client` so the compiler forces every caller
to supply one. Validate the address with `net.ParseIP` before the `inet` cast. Add the IP/user
agent to the security screen and consider a new-device notification.

---

## Phase 3 — P3

| Item | Action |
|---|---|
| F-14 reset delivery | Integrate SMTP/provider or disable the endpoint; never log plaintext tokens |
| F-21 dead code | Wire `logout-all` and/or delete `RegisterFailedLogin`, `EmailTaken`, `LogoutEverywhere`, `pendingPeers`, and the unused lockout columns |
| Room fairness | Cap rooms/participants per user; add host-only controls if intended |
| Last-admin guard | Refuse demoting the last admin |
| Metrics | Keep loopback default; document network policy |
| Documentation | Update `SECURITY.md` to state that desktop sessions are revoked and that socket handshakes are limited once fixed |

---

## Verification plan

For every changed item:

1. `gofmt -l cmd internal` → empty.
2. `go vet ./...` → clean.
3. `go test -race -p 1 ./...` → pass.
4. Targeted regression test from `REGRESSION_TESTS.md` → fail-before/pass-after.
5. `govulncheck ./...` → no reachable findings.
6. Manual flow against a local instance with synthetic accounts:
   - password change → old web and desktop credentials fail;
   - Google sign-in with an unverified local account → no silent link;
   - IPv6 login → success;
   - socket flood → 429/503;
   - AniList large response → not cached / bounded;
   - CORS wildcard → no credentials;
   - room code join attempts → throttled.
7. `docker compose config` for the dev stack; a production compose override for TLS/secrets.

Do not mark any finding resolved until steps 1–6 pass on the patched tree.

---

## Risk of the remediation itself

| Patch | Risk | Mitigation |
|---|---|---|
| Revoke desktop sessions on password change | Existing desktop clients must re-pair after a password change; expected UX | Communicate; keep the access-token TTL |
| Socket rate limit | A busy household/NAT could share a limit; HLS/playback unaffected because buckets are separate | Set generous burst, per-IP + global cap |
| AniList cache cap | Very large legitimate documents are re-fetched each time | 256 KiB is far above normal GraphQL responses; monitor hit rate |
| Fail-closed config | Existing dev scripts using `ENV` unset will stop booting | Update `.env.example`, compose, and CI; add a clear error message |
| Email verification | Adds a dependency on mail delivery; blocks Google linking until verified | Provide a CLI/admin verification path and a grace mode for a single-user deployment |
| IPv6 fix | Rate-limit keys change; existing windows reset | Expected; no data loss |
| Cookie rename | Existing sessions are logged out once | Keep a one-release read fallback |
| Refresh family rotation | A legitimate double-refresh can trigger re-auth | Short grace window; reuse detection only on a token older than the grace |
