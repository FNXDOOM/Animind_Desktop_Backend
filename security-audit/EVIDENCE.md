# Evidence Index (Redacted)

This file records the commands and observations behind the findings. It contains **no live
credentials, tokens, cookies, keys, passwords, PII or unredacted user data**.

---

## 1. Environment

```text
Platform : Windows (win32), PowerShell
Go       : go1.27.4 windows/amd64
Repo     : C:\Users\gudiy\Music\Animind_Backend_Go
Base     : bf58946 (main) + uncommitted working-tree changes
```

## 2. Static checks

### 2.1 Build and vet

```text
$ go vet ./...
(no output — success)
```

`go test ./...`:

```text
ok  animind-backend-go/cmd/server
ok  animind-backend-go/internal/auth
ok  animind-backend-go/internal/cache
ok  animind-backend-go/internal/config
ok  animind-backend-go/internal/downloads
ok  animind-backend-go/internal/httpx
ok  animind-backend-go/internal/media
ok  animind-backend-go/internal/metadata
ok  animind-backend-go/internal/repository
ok  animind-backend-go/internal/scanner
ok  animind-backend-go/internal/storage
ok  animind-backend-go/internal/syncplay
ok  animind-backend-go/internal/torrent
(all packages passed)
```

### 2.2 Module integrity

```text
$ go mod verify
all modules verified
```

### 2.3 Vulnerability scan

```text
$ go run golang.org/x/vuln/cmd/govulncheck@latest ./...
=== Symbol Results ===
Vulnerability #1: GO-2026-6974
  AttributeValueLengthLimit bypass via U+FFFD runes
  go.opentelemetry.io/otel/trace@v1.42.0  → fixed v1.47.0
Vulnerability #2: GO-2026-6278
  Gorilla WebSocket Uses Cryptographically Weak PRNG for WebSocket Mask Key
  github.com/gorilla/websocket@v1.5.3  → fixed v1.5.4-0.20240701034025-d67f41855da4
=== Package Results ===
Vulnerability #1: GO-2026-6975
  Memory allocation amplification in W3C baggage parsing
  go.opentelemetry.io/otel@v1.42.0  → fixed v1.47.0
Your code is affected by 2 vulnerabilities from 2 modules.
```

Full context in `DEPENDENCY_AUDIT.md` §3.

### 2.4 Secret scan

```text
$ git grep -n -I -E 'AKIA[0-9A-Z]{16}|-----BEGIN ... PRIVATE KEY|ghp_...|sk-...|xox[baprs]-'
(no matches)
$ git ls-files | Select-String -Pattern '(^|/)\.env($|\.)'
.env.example
$ git log --all --oneline --diff-filter=A -- '*.env' '*.key' '*.pem'
(no output)
```

A regex scan of the tracked Go/Python source found only test constants
(`const testPassword = "<test-only value>"` in `_test.go` files). No production secret is tracked.

### 2.5 `gosec`

The `gosec` run was started with `go run github.com/securego/gosec/v2/cmd/gosec@latest` and was
interrupted before completion. Treat it as **not run**; `REMEDIATION_PLAN.md` P2-5 adds it to CI.

## 3. IPv6 reproduction (F-07)

A standalone Go program (not added to the repo) reproduced the parsing difference:

```text
192.0.2.10:5555        cut="192.0.2.10"   correct="192.0.2.10"
[2001:db8::1]:5555     cut="[2001"        correct="2001:db8::1"
[::1]:5555             cut="["            correct="::1"
2001:db8::1            cut="2001"         correct="2001:db8::1"
```

`ClientIP` uses the `cut` column. `CreateWebSession` writes that value through
`nullif($5,'')::inet`, which rejects `[2001`.

## 4. Code evidence (redacted excerpts)

### F-01 — web-only revocation

```go
// internal/auth/service.go, ChangePassword (same shape in ResetPassword/ChangeEmail)
if err := s.store.RevokeAllWebSessions(ctx, userID, ""); err != nil {
    return err
}
s.InvalidateUser(ctx, userID)
```

```go
// internal/repository/users.go
update animind_web_sessions set revoked_at = now()
where user_id = $1::uuid and revoked_at is null and ($2 = '' or id::text <> $2)
```

`desktop_auth_sessions` is not referenced by either path. The desktop row is only revoked by the
explicit per-device routes or cleanup.

### F-02 — unthrottled socket mount

```go
// internal/httpx/server.go
if s.hub != nil {
    s.hub.Register(r)        // root router; per-route limiters are not applied here
}
```

```go
// internal/syncplay/hub.go
r.Get(SocketPath, h.serveSocket)
r.Head(SocketPath, h.serveSocket)
r.Post(SocketPath, h.serveSocket)
```

```go
// internal/syncplay/protocol.go — polling GET creates a peer before auth
p = newPeer(newSID(), "polling", h, h.logger)
h.register(p)
go p.keepAlive(h)
```

### F-03 — 4 MiB cached, entry-capped only

```go
upstream, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
...
s.rememberAniList(r.Context(), cacheKey, upstream)
```

```go
// internal/cache/local.go
const maxLocalEntries = 50_000
...
if len(l.entries) > maxLocalEntries { l.evictLocked() }
```

### F-04 — email-only linking

```go
// internal/auth/service.go
if existing, lookupErr := s.store.UserByEmailNormalized(ctx, email); lookupErr == nil {
    // Password account first, Google later: attach the identity to it.
    user = existing
}
```

`SignUp` (line 149) creates the account and calls `SignInUser` without any verification state.

### F-05 — environment-gated hardening

```go
Env: strings.ToLower(strings.TrimSpace(getString("ENV", getString("NODE_ENV", "development")))),
...
func (cfg *Config) validateProductionSecurity() error {
    if !cfg.IsProduction { return nil }
    ...
}
```

The sample signing key is 33 characters and passes `len(key) >= 32`. The webhook check rejects
only the literal `dev-secret`.

### F-09 — 5-char room code

```go
const roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
const roomCodeLength = 5      // 32^5 = 33,554,432
```

`handleJoinRoom` has no limiter; `ensureParticipant` authorizes playback control after a join.

### F-22 — client metadata never reaches the session row

```go
// internal/httpx/middleware.go — the only attachment for a normal request
ctx := auth.WithClient(r.Context(), auth.Client{
    UserAgent: r.UserAgent(),
    IPAddress: ClientIP(r, s.cfg.TrustProxy),
})
```

```go
// internal/auth/service.go — reads what the handler attached
s.store.CreateWebSession(ctx, userID, HashToken(token), deviceName,
    userAgent(ctx), clientIP(ctx), rememberMe, expiresAt)
```

`handleSignup` and `handleLogin` call `s.auth.SignUp`/`s.auth.Login` with a context that carries
only `WithLoginSource`; `handleGoogleCallback` and `handleConsumeCode` carry neither. The session
row is therefore written with an empty user agent and a `NULL` IP.

## 5. What is deliberately absent from this evidence

- No session cookie, bearer token, refresh token, bridge JWT, stream ticket or webhook secret.
- No password or password hash.
- No database connection string with credentials.
- No real user email, IP or watch history.
- No unredacted third-party provider response.
- No exploit payload that would be destructive or target a live system.

Any future reproduction should use throwaway accounts and synthetic media. If a secret must be
shown to prove a point, rotate it immediately after the review and never paste it into a report.
