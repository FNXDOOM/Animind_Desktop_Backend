# Remediation Status — 2026-11-25

**State:** code fixes applied to the working tree, not committed, not deployed.
**Verification:** `go vet` clean; `go test -race -p 1 ./...` passes; the full suite also passes
against a disposable PostgreSQL 16 container (migration `0004` applied); `govulncheck` reports
**no vulnerabilities**.

The pre-fix assessment remains in `SECURITY_FINDINGS.md`; this file records what changed after
it. Find `F-xx` IDs there for the evidence and original impact.

> **Second pass (admin console, 2026-11-25):** `ADMIN_PANEL_REVIEW.md` records the review of
> the new `/admin` panel and `/desktop-auth` pairing page. It fixed the legacy session-cookie
> fallback, added UUID validation to account actions, made access updates atomic, filtered
> desktop counts by absolute expiry, and removed unused scanner/grouping code.

---

## 1. Status by finding

| ID | Status | What changed | Verification |
|---|---|---|---|
| **F-01** | **Fixed** | `Store.RevokeAllDesktopSessions`; called by `ChangePassword`, `ResetPassword`, `ChangeEmail` in addition to the web-session revoke | `TestPasswordChangeRevokesDesktopSessions`, `TestResetPasswordRevokesDesktopSessions` (Postgres) |
| **F-02** | **Fixed** | `SocketHandshake` limiter mounted on the socket routes; `maxPeers=1000`; 10-second unauthenticated deadline on WebSocket and polling peers | `TestSocketHandshakeIsRateLimited`; cap/deadline code paths exercised by the suite |
| **F-03** | **Fixed** | `maxCachedAniListBytes=256 KiB` gate in the proxy; local cache now enforces a **64 MiB byte budget** with LRU-by-expiry eviction | `TestAniListProxyDoesNotCacheOversizeDocuments`, `TestLocalStoreEnforcesByteBudget`, `TestLocalStoreRefusesOversizeDocument` |
| **F-04** | **Mitigated** | `SignInWithGoogle` refuses to attach a Google identity to an existing **password** account (`ErrAccountLinkRequired`, 409). A pre-registered address can no longer inherit the victim's verified Google login. Email verification and mail delivery are still not implemented, so the squatting/denial aspect remains | `TestGoogleSignInRefusesPasswordAccountLink` (Postgres) |
| **F-05** | **Fixed** | `ENV` is required when `PUBLIC_URL` is not loopback; production rejects documented placeholder signing/webhook secrets; `DEBUG_ERRORS` defaults false; `.env.example` documents both | `TestProductionRejectsPlaceholderSecrets`, `TestLoadFailsClosedWithoutEnvironment`, `TestLoadAllowsLoopbackWithoutEnvironment` |
| **F-06** | **Partially fixed** | The 423 lockout is now returned after the same number of failures for an **unknown** address as for a real one, removing the login oracle. Signup still answers 409 for a taken address; that remains until email verification exists | `TestLockoutDoesNotRevealAccountExistence` (Postgres) |
| **F-07** | **Fixed** | `ClientIP` uses `net.SplitHostPort`; forwarded values are validated with `net.ParseIP` | `TestClientIPHandlesIPv6` |
| **F-08** | **Fixed** | `HLSSession.UserID`; `Create` records the owner; `Seek` rejects non-owners; `DestroyForUser`; heartbeat returns only that session's stats | `TestHLSManagerBindsControlToOwner`, `TestHLSManagerSeekRejectsNonOwner` |
| **F-09** | **Fixed** | Room code length 5 → 7; per-account failed-join window (10/min) with reset on success | `TestJoinFailureWindowBlocksGuessing`; functional room test updated to `roomCodeLength` |
| **F-10** | **Fixed** | Bridge token carries `wsid`; `DesktopExchange` requires the browser session row to still be active | `TestBridgeTokenRejectedAfterLogout` (Postgres) |
| **F-11** | **Fixed** | Migration `0004` adds `family_id`, `rotated_from_hash`, `absolute_expires_at` (90 days); rotation records the previous hash; replay revokes the family; absolute expiry is not extended | `TestRefreshTokenReuseRevokesFamily` (Postgres) |
| **F-12** | **Fixed** | Wildcard origin now returns `Access-Control-Allow-Origin: *` **without** credentials; the exact-allowlist path keeps credentials | `TestCORSWithWildcardNeverAllowsCredentials` |
| **F-13** | **Fixed** | `gorilla/websocket` upgraded to `v1.5.4-0.20240701034025-d67f41855da4`; otel modules to `v1.47.0`; `govulncheck` clean | `go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...` |
| **F-14** | **Open** | No mail/provider integration exists. The endpoint still stores only a token hash and cannot deliver it. This is an operator/product task, not a code defect that can be closed without a delivery channel | documented |
| **F-15** | **Fixed** | Secure deployments use `__Host-animind_session`; both names are cleared on logout and both are accepted on read for one rollout | `TestSessionCookieUsesHostPrefixWhenSecure` |
| **F-16** | **Mitigated** | `Manager.Create` serialises the count/insert decision with `quotaMu`; unique violations map to 409 | existing quota tests; multi-replica still needs a database advisory lock |
| **F-17** | **Fixed** | `Store.CanDownload` is a single-row query; the page loop is gone | compile + existing download tests |
| **F-18** | **Partially fixed** | GitHub Actions pinned to commit SHAs; `govulncheck` added to CI; runtime image moved from Alpine 3.20 to **3.22** (digest-pinned). Compose images remain development tags; no SBOM/signing yet | `go vet`, CI file review |
| **F-19** | **Fixed** | `safeDetail` is gated on `DEBUG_ERRORS`, default false in every environment | `TestDebugErrorDetailsAreOptIn` |
| **F-20** | **Fixed** | The recovery log line no longer contains the email or the token hash | code review |
| **F-21** | **Partially fixed** | Unused `pendingPeers` and the unused `EmailTaken` repository method removed. `RegisterFailedLogin`, `LogoutEverywhere` and the unused lockout columns remain (tests reference `RegisterFailedLogin`; `LogoutEverywhere` is useful) | compile + tests |
| **F-22** | **Fixed** | `auth.WithClient` is attached in signup, login, Google callback and auth-code consumption; session rows now record IP and user agent | `TestWebSessionRecordsClientMetadata` (Postgres) |

## 2. Potential findings

| ID | Status |
|---|---|
| P-01 (frontend-rendered subtitle/watchlist content) | **Open** — frontend not in this repository; server emits JSON with `nosniff` |
| P-02 (torrent write-time path escape) | **Open** — importer defence remains; upstream anacrolix storage is trusted |
| P-03 (ffmpeg parser RCE) | **Open** — inherent to untrusted media; sandbox/patch is a deployment control |
| P-04 (LLM prompt injection in scanner) | **Open** — no tools or cross-user data; integrity only |
| P-05 (public S3 bucket) | **Open** — cloud policy is outside the repository |
| P-06 (metrics exposure) | **Open** — loopback default; deployment control |

## 3. Files changed by the remediation

```
.env.example
.github/workflows/ci.yml
Dockerfile
go.mod / go.sum
internal/auth/jwt.go
internal/auth/service.go
internal/cache/local.go
internal/config/config.go
internal/config/config_test.go
internal/downloads/manager.go
internal/httpx/handler_auth.go
internal/httpx/handler_downloads.go
internal/httpx/handler_library.go
internal/httpx/handler_media.go
internal/httpx/middleware.go
internal/httpx/ratelimit.go
internal/httpx/responses.go
internal/httpx/server.go
internal/httpx/auth_flow_integration_test.go
internal/httpx/caching_test.go
internal/httpx/security_hardening_test.go
internal/media/hls.go
internal/media/hls_owner_test.go
internal/repository/desktopsessions.go
internal/repository/users.go
internal/syncplay/gate_test.go
internal/syncplay/hub.go
internal/syncplay/hub_join_test.go
internal/syncplay/protocol.go
internal/syncplay/room.go
internal/syncplay/rooms_functional_test.go
internal/cache/local_bytes_test.go
migrations/0004_desktop_session_families.sql
```

Plus the `security-audit/` documents. The user's pre-existing uncommitted changes to
`.env.example`, `SECURITY.md`, `docs/02-authentication.md`, `go.mod`, `go.sum`,
`internal/auth/auth_test.go`, `internal/auth/google.go`, `internal/auth/service.go`,
`internal/config/config.go`, `internal/config/config_test.go` were preserved; the remediation
edits are layered on top of them.

## 4. Compatibility notes

| Change | Client impact |
|---|---|
| `__Host-animind_session` | Only when `SECURE_COOKIES=true`. The old name is still read and cleared, so existing sessions survive one rollout |
| Google sign-in for an address with a password account | Now returns 409 `ErrAccountLinkRequired` instead of silently linking. A legitimate user must sign in with the password first. This is the intended security behaviour |
| Room codes 7 characters | Clients should treat the code as an opaque string. The bundled tests were updated |
| `/api/auth/me` limiter | 20/min → 240/min (`UserData` bucket). The old limiter made a normal SPA hit 429 |
| HLS heartbeat `stats` | Same JSON shape, but only the caller's session is included |
| Bridge token claims | Adds `wsid`; tokens minted before deployment have no `wsid` and fail exchange (they live 60 s) |
| Migration 0004 | Required before the new refresh flow; `DB_AUTO_MIGRATE=true` applies it at boot |

## 5. Remaining deployment actions

1. **TLS** in front of nginx and `SECURE_COOKIES=true`; the repository's nginx still listens on
   HTTP only.
2. Set real `DESKTOP_TOKEN_SIGNING_KEY`, `WEBHOOK_SECRET`, database and Grafana credentials;
   the production guard now rejects the documented placeholders but the operator must supply
   the values.
3. Decide the password-recovery channel (F-14) before letting anyone other than the operator
   sign up.
4. Keep `METRICS_ADDR` on loopback.
5. Apply `SCANNER_PRUNE_DELETED=false` if a transient mount loss must not prune the catalog.
6. Pin the remaining compose images by digest and add an SBOM/signing step when the deployment
   is promoted beyond local development.

## 6. Verification log

```
$ go vet ./...
(clean)

$ go test -race -p 1 ./...
ok  animind-backend-go/...   (all packages, integration skipped without a database)

$ docker run -d --rm --name animind-audit-pg ... postgres:16-alpine
$ POSTGRES_TEST_DATABASE_URL=... go test -count=1 -p 1 ./...
ok  animind-backend-go/internal/auth
ok  animind-backend-go/internal/httpx
ok  animind-backend-go/internal/repository
ok  animind-backend-go/internal/media
ok  animind-backend-go/internal/syncplay
ok  animind-backend-go/internal/downloads
(and all other packages)

$ go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
No vulnerabilities found.
```

The disposable container was stopped and removed after the run. No data was deleted from the
project, no commit was created, and nothing was deployed.
