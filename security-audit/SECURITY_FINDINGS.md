# Security Findings

**Status vocabulary**

| Status | Meaning |
|---|---|
| **Confirmed** | The code path is present and the control failure is proven by source analysis; a local regression test is proposed |
| **Potential** | A sink exists but impact depends on a component outside this repository or on a deployment decision; not counted in the severity totals |
| **False positive** | Examined and rejected; recorded so it is not re-reported |
| **N/A** | The class does not exist in this codebase |

All line numbers refer to the audit snapshot (commit `bf58946` + the uncommitted working-tree
hardening changes). Secrets are never reproduced; only hashes or placeholders appear.

> **Remediation update (2026-11-25):** the code fixes are applied and verified; see
> `REMEDIATION_STATUS.md` for which findings are fixed, mitigated or still open. The evidence
> below is the pre-fix assessment and is intentionally preserved for the record.

---

## F-01 — Desktop credentials survive password reset, password change and email change

| Field | Value |
|---|---|
| **Severity** | **High** |
| **Status** | Confirmed |
| **CWE** | CWE-613 (Insufficient Session Expiration); CWE-620 (Unverified Password Change) |
| **OWASP** | A07:2021 Identification and Authentication Failures |
| **CVSS 3.1** | 8.1 — `AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:N` (conditional on prior credential theft; see §Impact) |
| **Skills** | `hunt-session`, `hunt-ato`, `hunt-auth-bypass`, `hunt-mfa-bypass` |
| **Affected** | `internal/auth/service.go:632-671` (`ChangeEmail`), `:673-701` (`ChangePassword`), `:725-745` (`ResetPassword`); `internal/repository/users.go:372-377`; `internal/repository/desktopsessions.go:150-160` |

### Root cause

All three credential-remediation paths call only `RevokeAllWebSessions`:

```go
// internal/auth/service.go — ChangePassword (same shape in ResetPassword / ChangeEmail)
if err := s.store.RevokeAllWebSessions(ctx, userID, ""); err != nil {
    return err
}
s.InvalidateUser(ctx, userID)
```

`RevokeAllWebSessions` updates `animind_web_sessions` only:

```go
// internal/repository/users.go
update animind_web_sessions set revoked_at = now()
where user_id = $1::uuid and revoked_at is null and ($2 = '' or id::text <> $2)
```

`desktop_auth_sessions` is never touched. The only code that revokes desktop rows is the explicit
per-device `RevokeSession`/`DesktopRevoke` path and the daily cleanup, which deletes only
already-revoked or refresh-expired rows.

### Why this is exploitable, not theoretical

1. `POST /api/auth/desktop/exchange` accepts the browser session cookie
   (`credential(r)` in `handler_auth.go:425`) and mints a desktop access + **30-day refresh**
   pair. A stolen cookie therefore becomes a long-lived desktop credential.
2. `DesktopRefresh` rotates the pair and renews `refreshExpires` to `now + 30 days`
   (`service.go:509-513`). A stolen refresh token can be kept alive indefinitely.
3. After the victim changes or resets the password, the attacker's desktop row is still valid;
   `DesktopRefresh` succeeds and issues a fresh access token.
4. The victim has no signal that the device is still paired unless they open the session list and
   recognise it.

This defeats the primary purpose of a password reset after a suspected compromise. The
`SECURITY.md` statement that "every other web session is revoked" is true for web sessions and
silently incomplete for the desktop family.

### Impact

An attacker who obtains a web session or a desktop refresh token retains full account access
(profile, watch history, watchlist, downloads if entitled, watch-party control, session list)
after the victim changes or resets the password. Because refresh extends the window, access can
be maintained for as long as the attacker keeps refreshing. CVSS assumes the token is already
stolen; the vulnerability is the failure to revoke, not the theft itself.

### Reproduction (local, synthetic)

1. Create user A and sign in; capture the `as_` session cookie.
2. `POST /api/auth/desktop/exchange` with that cookie; store the `adr_` refresh token.
3. Change A's password via `POST /api/auth/password/change` (or reset it).
4. `POST /api/auth/desktop/refresh` with the stored refresh token.
5. Observe: HTTP 200 with a new access/refresh pair; the desktop session still works.

### Remediation

Add a repository method that revokes every desktop row for a user, and call it from all three
paths:

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

```go
// internal/auth/service.go — ChangePassword / ResetPassword / ChangeEmail
if err := s.store.RevokeAllWebSessions(ctx, userID, keepSessionID); err != nil { ... }
if err := s.store.RevokeAllDesktopSessions(ctx, userID); err != nil { ... }
```

Consider also: (a) an absolute maximum lifetime for a refresh-token family (F-11), (b) requiring
recent re-authentication before `desktop/exchange` if the account's password changed since the
web session was created, and (c) a notification to the account email on password change.

### Regression test

`REGRESSION_TESTS.md` §1 (`TestPasswordChangeRevokesDesktopSessions`) and §2
(`TestResetPasswordRevokesDesktopSessions`). Both fail against current code.

---

## F-02 — Unauthenticated Socket.IO connections are unlimited and unthrottled

| Field | Value |
|---|---|
| **Severity** | **High** (availability only) |
| **Status** | Confirmed |
| **CWE** | CWE-770 (Allocation of Resources Without Limits or Throttling); CWE-400 |
| **OWASP** | A05:2021 Security Misconfiguration / API4:2023 Unrestricted Resource Consumption |
| **CVSS 3.1** | 7.5 — `AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H` |
| **Skills** | `hunt-websocket`, `hunt-brute-force`, `hunt-business-logic` |
| **Affected** | `internal/httpx/server.go:290-293`; `internal/syncplay/hub.go:78-108`; `internal/syncplay/protocol.go:337-361`, `:563-658` |

### Root cause

The hub is mounted directly on the root router:

```go
// internal/httpx/server.go
if s.hub != nil {
    s.hub.Register(r)          // root chi.Router — no rate limiter middleware
}
```

```go
// internal/syncplay/hub.go
func (h *Hub) Register(r chi.Router) {
    if !h.cfg.SyncPlay.Enabled { return }
    r.Get(SocketPath, h.serveSocket)
    r.Head(SocketPath, h.serveSocket)
    r.Post(SocketPath, h.serveSocket)
}
```

`serveSocket` accepts:
- a WebSocket upgrade with no authentication at upgrade time (auth happens later in the
  Socket.IO `CONNECT` packet), and
- `GET /api/socket.io?EIO=4&transport=polling`, which immediately creates a peer, registers it in
  `h.peers`, starts a `keepAlive` goroutine, and returns the Engine.IO OPEN frame.

There is no:
- per-IP limiter on the upgrade/polling handshake,
- maximum number of unauthenticated peers,
- authentication deadline (a client can answer Engine.IO pings and hold a WebSocket forever),
- `MaxParticipants`/`MaxPeers` for peers outside a room.

The `SECURITY.md` claim that "the global middleware chain still applies" is true for
logging/recovery/headers but **not** for the per-route rate limiters, because those are attached
per route and the socket routes are registered separately.

### Impact

An unauthenticated client can consume file descriptors, goroutines, per-peer channels (64-frame
buffer) and poll state until the process or nginx worker hits its limit. A blocked admin UI,
failed media playback and a restart are all realistic. nginx's default `worker_connections` is
also reachable, so the proxy can be taken down before the Go process.

### Reproduction (local, synthetic)

1. Start the server with `SYNCPLAY_ENABLED=true`.
2. Open `GET /api/socket.io?EIO=4&transport=polling` in a loop; each response contains an OPEN
   frame with a new `sid`.
3. Observe `animind_watchparty_peers` / `hg.PeerCount()` grow without bound.
4. Hold one WebSocket per connection and reply to pings; the peer is never expired.

### Remediation

1. Mount the socket routes behind a per-IP limiter (for example
   `s.rate(s.limiter.PublicRead)` or a dedicated `SocketHandshake` bucket) and a global
   semaphore of unauthenticated peers.
2. Close a connection that has not sent a valid `CONNECT` within 10 seconds.
3. Cap `h.peers` (e.g. 1 000) and return 503/close with a reason once reached.
4. Add nginx `limit_conn`/`limit_req` on `/api/socket.io` as a second layer.
5. Add a metric for pre-auth peers separately from authenticated peers.

### Regression test

`REGRESSION_TESTS.md` §6 (`TestSocketHandshakeIsRateLimited`) and §7
(`TestPreAuthSocketIsClosedWithoutConnect`).

---

## F-03 — Public AniList proxy can exhaust process memory through its cache

| Field | Value |
|---|---|
| **Severity** | **Medium** (High availability impact with sustained effort) |
| **Status** | Confirmed |
| **CWE** | CWE-770 / CWE-400 |
| **OWASP** | API4:2023 Unrestricted Resource Consumption |
| **CVSS 3.1** | 5.3 — `AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L` (worst case 7.5 if the cache is driven to its entry cap) |
| **Skills** | `hunt-cache-poison`, `hunt-api-misconfig`, `performance` |
| **Affected** | `internal/httpx/handler_library.go:261-374`; `internal/cache/local.go:28-30`, `:104-128`; `internal/cache/cache.go:156-159`; route at `internal/httpx/server.go:189` |

### Root cause

`POST /api/anilist` is unauthenticated (rate-limited to 120/min/IP), reads up to **4 MiB** from
AniList, and caches a successful document for `CACHE_METADATA_TTL_HOURS` (12 h default):

```go
upstream, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
...
s.rememberAniList(r.Context(), cacheKey, upstream)
```

The key is a hash of the normalized query, so every distinct query creates a distinct entry. The
local store caps the **number** of entries at 50 000 and evicts by expiry, but it does not cap
**bytes**:

```go
const maxLocalEntries = 50_000
...
l.entries[key] = &localEntry{value: value, expiresAt: l.now().Add(ttl)}
if len(l.entries) > maxLocalEntries { l.evictLocked() }
```

A single 4 MiB response is 4 MiB of live heap; 50 000 of them is 200 GiB. In practice an attacker
does not need the theoretical maximum: a few hundred large documents can push a small VPS into
OOM. Redis deployments are bounded only if the operator configured `maxmemory`; the bundled
compose does (192 MiB), a bare Redis does not.

### Impact

Process crash or OOM kill; loss of in-flight media streams, HLS sessions and watch-party state.
No data disclosure or integrity impact.

### Reproduction (local, synthetic)

1. Start with no `REDIS_URL` (default local cache).
2. Send distinct valid AniList queries whose responses are large (e.g. wide media pages).
3. Observe RSS grow by the response size per distinct query; `animind_cache_operations_total`
   shows the writes; the entry cap does not evict until 50 000 entries.

### Remediation

- Cap the bytes stored per document (e.g. 256 KiB) and skip caching larger responses.
- Add a total byte budget to the local store with LRU eviction, not just an entry count.
- Optionally require authentication for `/api/anilist`, or restrict it to a small allowlist of
  known query shapes.
- Keep Redis `maxmemory` + `volatile-lru` as a deployment requirement, not an assumption.

### Regression test

`REGRESSION_TESTS.md` §8 (`TestAniListCacheRefusesOversizeDocument`).

---

## F-04 — Unverified signup + Google email linking enables pre-account takeover

| Field | Value |
|---|---|
| **Severity** | **Medium** |
| **Status** | Confirmed (conditional on the attacker registering before the victim) |
| **CWE** | CWE-287 (Improper Authentication); CWE-620 (Unverified Password Change / identity) |
| **OWASP** | A07:2021 Identification and Authentication Failures |
| **CVSS 3.1** | 6.8 — `AV:N/AC:H/PR:N/UI:R/S:U/C:H/I:H/A:N` |
| **Skills** | `hunt-ato`, `hunt-auth-bypass`, `hunt-oauth` |
| **Affected** | `internal/auth/service.go:149-183` (`SignUp`), `:877-924` (`SignInWithGoogle`); `internal/httpx/handler_auth.go:43-62`; `internal/auth/google.go:171-179` |

### Root cause

Signup accepts any well-formed address and immediately issues a session; no verification message
is sent and no `email_verified` state exists on `animind_users`. Later, Google sign-in links to an
existing local account purely on the normalized email:

```go
if existing, lookupErr := s.store.UserByEmailNormalized(ctx, email); lookupErr == nil {
    // Password account first, Google later: attach the identity to it.
    user = existing
}
...
s.store.LinkIdentity(ctx, user.ID, "google", profile.Subject, ...)
```

The recent hardening (uncommitted) correctly requires Google to assert `email_verified=true`.
That protects against a forged Google claim, but it does **not** protect against the reverse
direction: the local account was created by someone who never proved control of the address.

### Attack sequence

1. Victim has not yet registered. `ALLOW_SIGNUP=true` (default).
2. Attacker registers `victim@example.com` with a password the attacker chooses.
3. Victim later clicks "Continue with Google". Google returns a verified `victim@example.com`.
4. `SignInWithGoogle` finds the attacker-created row by email and links the Google subject to it.
5. The victim is now using an account whose password the attacker knows. The attacker can sign in
   with the password at any time, read the victim's watch history/preferences, control watch
   parties, and (if entitled) start downloads in the victim's name. The victim cannot reset the
   password because reset delivery is not implemented (F-14), and the attacker can change the
   email first if they obtain a session.

A second, simpler consequence: an attacker can squat any address and block the real owner from
registering it, or use signup to enumerate addresses (F-06).

### Impact

Account takeover of a victim who uses Google sign-in, with a precondition that the attacker
registered the address first. The victim's data and in-app identity are exposed; the attacker
keeps a working password. Conditional, but the conditions are ordinary (public email address,
signup open).

### Remediation

- Add an email-verification state and do not issue a usable session (or mark the account
  `unverified` and restrict it) until the address is confirmed.
- When linking a provider to an **existing password account**, require proof of the existing
  credential (password login) or send a confirmation link to the existing address before the
  link is created.
- Do not auto-link solely on email for accounts that were never verified.
- Consider disabling password signup (`ALLOW_SIGNUP=false`) for single-operator deployments, as
  the hardening checklist already recommends.

### Regression test

`REGRESSION_TESTS.md` §9 (`TestGoogleLinkRequiresExistingAccountProof`) and §10
(`TestUnverifiedSignupCannotBeUsed`).

---

## F-05 — Production hardening is bypassed when `ENV` is unset, and sample secrets pass validation

| Field | Value |
|---|---|
| **Severity** | **Medium** (High if deployed with the sample key and `ENV` unset) |
| **Status** | Confirmed |
| **CWE** | CWE-1188 (Insecure Default Initialization of Resource); CWE-16 (Configuration) |
| **OWASP** | A05:2021 Security Misconfiguration |
| **CVSS 3.1** | 7.4 — `AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:N` (configuration-dependent; see §Impact) |
| **Skills** | `hunt-tls-network`, `hunt-exceptional-conditions`, `hunt-cloud-misconfig` |
| **Affected** | `internal/config/config.go:348`, `:385-407`, `:433-464`; `.env.example:13`, `:40`, `:76`; `docker-compose.yml:52`, `:76-77`, `:94`, `:168`; `internal/httpx/handler_auth.go:600-610` |

### Root cause

`ENV` defaults to `development`:

```go
Env: strings.ToLower(strings.TrimSpace(getString("ENV", getString("NODE_ENV", "development")))),
...
cfg.IsProduction = cfg.Env == "production"
```

Every hardening check is gated on `IsProduction`:

```go
func (cfg *Config) validateProductionSecurity() error {
    if !cfg.IsProduction { return nil }
    if !cfg.Auth.SecureCookies { return errors.New("SECURE_COOKIES must be true in production") }
    for _, origin := range cfg.FrontendOrigins {
        if origin == "*" { return errors.New("FRONTEND_URL must not include * in production") }
    }
    return nil
}
```

Consequences of a deployment that omits `ENV=production` (a copied `.env.example` does exactly
this):

| Setting | Value | Effect |
|---|---|---|
| `SECURE_COOKIES` | defaults to `IsProduction` → **false** | session cookie can travel over HTTP; SSL-strip/downgrade theft |
| `FRONTEND_URL` | `*` accepted | CORS reflects any origin with credentials (F-12) |
| `WEBHOOK_SECRET` | `dev-secret` accepted | anyone can trigger scans (`webhook` route, 30/min) |
| Error details | `safeDetail` returns `err.Error()` when not production | SQL/path/provider error text reaches clients |
| `DESKTOP_TOKEN_SIGNING_KEY` | sample value is 33 chars | passes the `len >= 32` check; tokens forgeable if known |

The production check also does not reject the documented placeholders:
`replace-with-a-long-random-secret` (33 chars) and `change-me` pass `Load()`. It only rejects the
literal `dev-secret`. `docker-compose.yml` ships a 49-char development signing key, a webhook
secret, `ENV=development`, and `POSTGRES_PASSWORD` equal to the username; the nginx port is
published on all interfaces.

### Impact

If the stack is copied to a VPS and `ENV` is not changed:

- a network attacker can steal a session cookie over the plaintext nginx listener;
- any website can make credentialed CORS reads (limited by `SameSite=Lax`, see F-12);
- an attacker who learns the published sample key can mint bridge JWTs and exchange them for
  desktop sessions;
- 500 responses disclose internal error text (paths, SQL fragments, provider messages).

This is a deployment/defaults flaw, not a bypass of a correctly configured production instance.

### Remediation

- Require an explicit environment: fail startup if `ENV` is unset and `PUBLIC_URL` is not a
  loopback address.
- Default `SECURE_COOKIES=true`; require an explicit `INSECURE_COOKIES=true` for local HTTP.
- Reject known placeholder values for every secret in every environment (substring list:
  `change-me`, `replace-with`, `local-development`, `dev-secret`, `password`).
- Reject `FRONTEND_URL=*` unconditionally; if a wildcard is needed for local tooling, require an
  explicit `ALLOW_INSECURE_CORS=true` **and** never send credentials with it.
- Return error `details` only with an explicit `DEBUG_ERRORS=true`, never based on `ENV`.
- Make the compose file clearly non-deployable (e.g. a separate `docker-compose.dev.yml`) and
  bind nginx to `127.0.0.1` by default.

### Regression test

`REGRESSION_TESTS.md` §11 (`TestConfigRejectsPlaceholderSecrets`) and §12
(`TestConfigFailsClosedWithoutEnvironment`).

---

## F-06 — Account existence is disclosed by lockout and signup responses

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-204 (Observable Response Discrepancy) / CWE-203 |
| **OWASP** | A07:2021 Identification and Authentication Failures |
| **CVSS 3.1** | 5.3 — `AV:N/AC:L/PR:N/UI:N/S:U/C:L/I:N/A:N` |
| **Skills** | `hunt-brute-force`, `hunt-auth-bypass` |
| **Affected** | `internal/auth/service.go:188-264`; `internal/httpx/handler_auth.go:43-62`, `:574-596` |

### Root cause

Unknown emails always receive `401 InvalidCredentials` after a dummy argon2 run. Existing emails
that fail `AUTH_MAX_FAILED_LOGINS` (default 10) additionally receive `423`/`ErrAccountLocked`
from `recordLoginFailure`. The failure counter is keyed by `(email, source)` and is only written
for an existing row, so:

- 1–9 wrong passwords → 401 (same as unknown);
- 10th wrong password → 423 only if the account exists.

Signup returns `409`/`ErrEmailTaken` for a taken normalized email, which is a one-request oracle.

### Impact

An attacker can enumerate registered addresses and target them for credential stuffing or
phishing. Enumeration is also a prerequisite for F-04. No direct account access.

### Reproduction (local, synthetic)

1. `POST /api/auth/login` ten times with `known@example.com` and a wrong password → 423 on the
   tenth (if registered) vs 401 for `nobody@example.com`.
2. `POST /api/auth/signup` with an existing address → 409.

### Remediation

- Return the same status/body for "locked" and "invalid" and let the generic rate limiter carry
  the delay; log the lockout server-side.
- For signup, either (a) always answer `202` and send a verification email (preferred, and
  required for F-04), or (b) accept the inherent disclosure and rate-limit heavily.
- Do not include `ErrEmailTaken` text in the response if a generic message is acceptable.

### Regression test

`REGRESSION_TESTS.md` §13 (`TestLockoutDoesNotRevealAccountExistence`).

---

## F-07 — `ClientIP` mishandles IPv6, collapsing rate-limit and lockout keys

| Field | Value |
|---|---|
| **Severity** | **Low** (control weakening) |
| **Status** | Confirmed |
| **CWE** | CWE-20 (Improper Input Validation); CWE-703 (Improper Check or Handling of Exceptional Conditions) |
| **OWASP** | A05:2021 Security Misconfiguration |
| **CVSS 3.1** | 5.3 — `AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:L/A:N` |
| **Skills** | `hunt-brute-force`, `hunt-exceptional-conditions` |
| **Affected** | `internal/httpx/responses.go:147-164` |

### Root cause

```go
host, _, found := strings.Cut(r.RemoteAddr, ":")
if !found { return r.RemoteAddr }
return host
```

`RemoteAddr` for IPv6 is `[2001:db8::1]:1234`. Cutting at the first colon yields `[2001`, not the
address. Verified locally:

| `RemoteAddr` | current `ClientIP` | correct |
|---|---|---|
| `192.0.2.10:5555` | `192.0.2.10` | `192.0.2.10` |
| `[2001:db8::1]:5555` | `[2001` | `2001:db8::1` |
| `[::1]:5555` | `[` | `::1` |

Every IPv6 address whose first hextet matches shares a rate-limit bucket, and the login-failure
lockout key (`WithLoginSource`) is built from the same value. Consequences:

- unrelated IPv6 clients throttle or lock each other out;
- an attacker on a `2001:*` address is not isolated from other `2001:*` clients, so the per-source
  brute-force budget is effectively shared and can be exhausted for everyone in that prefix;
- the value is unusable as an identity if it is ever persisted (it is not today — see F-22).

The `X-Forwarded-For` path (used behind the bundled nginx) is unaffected because nginx supplies a
bare IPv6 address.

### Remediation

```go
func ClientIP(r *http.Request, trustProxy bool) string {
    if trustProxy { /* existing header handling; also validate with net.ParseIP */ }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil { return r.RemoteAddr }
    return host
}
```

Validate forwarded values with `net.ParseIP` and fall back to `RemoteAddr` when parsing fails.

### Regression test

`REGRESSION_TESTS.md` §5 (`TestClientIPHandlesIPv6`).

---

## F-08 — HLS session controls are not bound to the session owner

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed (requires knowledge of a 128-bit session id) |
| **CWE** | CWE-639 (Authorization Bypass Through User-Controlled Key); CWE-862 |
| **OWASP** | API1:2023 Broken Object Level Authorization |
| **CVSS 3.1** | 5.4 — `AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:L/A:L` |
| **Skills** | `hunt-idor`, `hunt-websocket` |
| **Affected** | `internal/httpx/handler_media.go:466-520`; `internal/media/hls.go:440-471` |

### Root cause

`handleHLSSeek`, `handleHLSHeartbeat` and `handleDestroyHLSSession` require authentication but
only check `s.hls.Has(sessionID)`. The session object records `EpisodeID` but no owning user.
`handleHLSHeartbeat` returns `s.hls.Stats()`, which lists every session (id, episode id, audio
track, start time, last access) to any authenticated caller that presents any live session id.

### Impact

Anyone who learns another viewer's HLS session id (shared playlist URL, proxy log, screenshot,
referer) can seek or destroy that viewer's stream, or enumerate all active sessions' episode ids
and timing. It does not expose media bytes beyond what the unauthenticated playlist/segment URLs
already expose, and the id is 128-bit random, so this is a low-likelihood authorization gap.

### Remediation

Store `UserID` in `HLSSession` at creation (from the authenticated principal), and check it in
seek/heartbeat/destroy. Do not return global `Stats()` on a per-session heartbeat; return only
that session's state.

### Regression test

`REGRESSION_TESTS.md` §14 (`TestHLSSessionCannotBeControlledByAnotherUser`).

---

## F-09 — Watch-party room codes can be brute-forced without throttling

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-307 (Improper Restriction of Excessive Authentication Attempts); CWE-640 |
| **OWASP** | A07:2021 Identification and Authentication Failures |
| **CVSS 3.1** | 5.4 — `AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:L/A:N` |
| **Skills** | `hunt-brute-force`, `hunt-websocket` |
| **Affected** | `internal/syncplay/hub.go:352-386`; `internal/syncplay/room.go:627-698`, `:713-817`; `internal/httpx/handler_library.go:662-683` |

### Root cause

Room codes are 5 characters from a 32-character alphabet (`32^5 = 33 554 432`). `handleJoinRoom`
performs no per-user or per-IP throttle. The HTTP lookup is limited (120/min/IP), but the socket
event path is not. Joining makes the peer a participant, and `ensureParticipant` authorizes
`play`, `pause`, `seek`, `changeEpisode`, `transferHost` and the buffering/gate events.

### Impact

An authenticated attacker who guesses an active code can join a private watch party, see the
participant list and episode, and pause/seek/change the episode for everyone. With 100 active
rooms, the expected guesses per hit is ~335 000; at a few hundred events/second over one socket
this is minutes-to-hours, not years. No confidentiality of media (the library is shared), but the
room's social state and playback are exposed.

### Remediation

- Increase the code space to 7–8 characters (32^8 ≈ 1.1 × 10^12).
- Rate-limit failed `joinRoom` attempts per authenticated user and per IP, with a short lockout.
- Require host approval or an optional room password for private parties.
- Remove `HostUserID` from the unauthenticated-adjacent lookup response or return a display name
  only.

### Regression test

`REGRESSION_TESTS.md` §15 (`TestJoinRoomIsRateLimited`).

---

## F-10 — Bridge token remains exchangeable for 60 seconds after logout

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-613 (Insufficient Session Expiration) |
| **OWASP** | A07:2021 |
| **CVSS 3.1** | 4.1 — `AV:N/AC:H/PR:L/UI:N/S:U/C:L/I:L/A:N` |
| **Skills** | `hunt-session`, `hunt-ato` |
| **Affected** | `internal/auth/service.go:443-458`, `:574-587` |

### Root cause

`DesktopExchange` first tries `Authenticate`; when that fails it falls back to
`VerifyBridgeTokenForIssuer`, which validates signature, audience, issuer and expiry only. It
does not check that the web session that minted the bridge token is still active. The bridge
token's lifetime is 60 seconds.

### Impact

If a bridge token leaks (loopback callback URL in a browser history, shared-host log, crash
report) and the user logs out, there is a 60-second window in which the token can still be
exchanged for a 30-day desktop pair. Low likelihood, but it compounds F-01 because the resulting
desktop pair outlives logout anyway.

### Remediation

Include the web session row id in the bridge claims (the code already has
`principal.WebSessionRowID`) and verify the row is live (`revoked_at is null and expires_at > now()`)
before issuing desktop tokens. Alternatively make the exchange, not just the token, one-time by
consuming a nonce from the cache.

### Regression test

`REGRESSION_TESTS.md` §16 (`TestBridgeTokenRejectedAfterLogout`).

---

## F-11 — Refresh-token rotation has no reuse detection and slides indefinitely

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-613 (Insufficient Session Expiration); CWE-384 (Session Fixation family) |
| **OWASP** | A07:2021 |
| **CVSS 3.1** | 5.4 — `AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:L/A:N` |
| **Skills** | `hunt-session`, `hunt-jwt-crypto` |
| **Affected** | `internal/auth/service.go:489-530`; `internal/repository/desktopsessions.go:63-81` |

### Root cause

`DesktopRefresh` rotates the refresh hash in place and sets `refreshExpires = now + 30 days` on
every rotation. Replaying a rotated-out token returns 401, but there is no token-family state and
no reuse response: the row's new token remains valid, and the legitimate client is simply logged
out. There is no absolute maximum age.

### Impact

A stolen refresh token that the attacker refreshes first gives persistent access; the victim's
next refresh fails, which looks like an ordinary sign-out. Without reuse detection the server
cannot distinguish "stolen token replayed" from "client retried", and neither the account nor
other devices are revoked.

### Remediation

- Give each desktop session a `family_id` and an absolute `session_expires_at` (e.g. 90 days).
- On refresh, if the presented hash does not match the current row but matches a
  previously-rotated hash, revoke the entire family and require re-authentication.
- Keep a short grace window for legitimate double-refresh races, but do not renew the absolute
  lifetime.
- Log and optionally notify on reuse.

### Regression test

`REGRESSION_TESTS.md` §17 (`TestRefreshReuseRevokesFamily`).

---

## F-12 — Wildcard CORS reflects any origin with credentials outside production

| Field | Value |
|---|---|
| **Severity** | **Low** (defense-in-depth; `SameSite=Lax` limits cross-site cookie sends) |
| **Status** | Confirmed |
| **CWE** | CWE-942 (Permissive Cross-domain Policy with Untrusted Domains) |
| **OWASP** | A05:2021 |
| **CVSS 3.1** | 3.1 — `AV:N/AC:H/PR:N/UI:R/S:U/C:L/I:N/A:N` |
| **Skills** | `hunt-cors`, `hunt-csrf` |
| **Affected** | `internal/httpx/middleware.go:75-97`; `internal/config/config.go:740-750` |

### Root cause

`OriginAllowed` returns true when the configured list contains `"*"`, and the CORS middleware
reflects the request `Origin` and sets `Access-Control-Allow-Credentials: true`. Production
rejects `"*"`, but non-production (including the default `ENV`, see F-05) accepts it.

### Impact

A malicious site could send credentialed cross-origin requests and read the responses **if** the
browser attaches the session cookie. The cookie is `SameSite=Lax`, so ordinary cross-site
`fetch` does not attach it; the practical impact is therefore latent. It becomes real if the
cookie policy is later relaxed to `None` (for a separate frontend origin) or if a browser/edge
behavior changes. It also allows any origin to read unauthenticated endpoints.

### Remediation

Never combine a reflected origin with `Allow-Credentials: true`. If `*` is configured, return
`Access-Control-Allow-Origin: *` and omit credentials. Reject `*` in every environment; use an
explicit origin list even for local development.

### Regression test

`REGRESSION_TESTS.md` §18 (`TestCORSWithWildcardNeverAllowsCredentials`).

---

## F-13 — Reachable dependency vulnerabilities

| Field | Value |
|---|---|
| **Severity** | **Low** (published advisories are Medium; no direct exploit path demonstrated) |
| **Status** | Confirmed (reachable) |
| **CWE** | CWE-1104 (Use of Unmaintained Third-Party Components); CWE-338 (Weak PRNG) |
| **Skills** | `supply-chain-attack-recon`, `hunt-cicd` |
| **Affected** | `go.mod:13` (`github.com/gorilla/websocket v1.5.3`), `:114-116` (`go.opentelemetry.io/otel* v1.42.0`) |

`govulncheck` (run against the working tree) reports:

| Advisory | Module | Found | Fixed | Note |
|---|---|---|---|---|
| GO-2026-6278 | `github.com/gorilla/websocket` | v1.5.3 | v1.5.4-0.20240701034025-d67f41855da4 | weak PRNG for WebSocket masking keys |
| GO-2026-6974 | `go.opentelemetry.io/otel/trace` | v1.42.0 | v1.47.0 | `AttributeValueLengthLimit` bypass via U+FFFD |
| GO-2026-6975 | `go.opentelemetry.io/otel` | v1.42.0 | v1.47.0 | memory amplification in baggage parsing (package-level; not called) |

`gorilla/websocket` is used by the watch-party hub; `otel` is pulled in indirectly by the torrent
client. The OTel baggage issue is not called by this code, and the mask-key weakness is not an
authentication control, which is why the combined severity is Low.

### Remediation

```bash
go get github.com/gorilla/websocket@latest
go get go.opentelemetry.io/otel@v1.47.0 go.opentelemetry.io/otel/trace@v1.47.0 \
      go.opentelemetry.io/otel/metric@v1.47.0
go mod tidy && go test ./...
```

Add `govulncheck ./...` to CI and fail on reachable findings. Full table in
`DEPENDENCY_AUDIT.md`.

---

## F-14 — Password reset cannot deliver a token

| Field | Value |
|---|---|
| **Severity** | **Informational** (operational safety; would be High if plaintext tokens were logged) |
| **Status** | Confirmed by design/documentation |
| **CWE** | CWE-640 (Weak Password Recovery Mechanism) |
| **Skills** | `hunt-forgot-password`, `hunt-ato` |
| **Affected** | `internal/httpx/handler_auth.go:156-180`; `SECURITY.md:83-85` |

`RequestPasswordReset` generates a 32-byte token and stores its SHA-256, but the handler logs only
`token_hash` and returns a generic 202. There is no mail transport. A user therefore cannot
complete a reset unless an external integration reads the database/queue — and the hash is not
enough to reconstruct the token. This is documented in `SECURITY.md` ("delivery is not
included"), but it means the recovery flow is effectively disabled.

The security risk is the temptation to "fix" it by logging the plaintext token: anyone with log
access could then reset any account. If that change is ever made, this becomes a High-severity
ATO-by-log-access issue.

### Recommendation

Implement a delivery channel (SMTP/provider) before enabling recovery for real users, or remove
the endpoint until one exists. Never log the plaintext token; if an operator-assisted reset is
needed, provide a CLI that reads a one-time token from the database and prints it to a TTY, not
to a log sink.

---

## F-15 — Session cookie lacks the `__Host-` prefix

| Field | Value |
|---|---|
| **Severity** | **Informational / Low** (hardening) |
| **Status** | Confirmed |
| **CWE** | CWE-1275 (Sensitive Cookie with Improper SameSite Attribute) / cookie scope |
| **Skills** | `hunt-session`, `hunt-subdomain` |
| **Affected** | `internal/httpx/server.go:34-35`, `:391-412` |

The cookie is `animind_session`, `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` conditionally, and
no `Domain` attribute (host-only, which is good). A sibling subdomain can still set a cookie with
the same name for its own host or a parent domain in some configurations, producing session
confusion/fixation. The `__Host-` prefix (requires `Secure`, `Path=/`, no `Domain`) prevents a
subdomain from overwriting it.

### Recommendation

Rename to `__Host-animind_session` once `SECURE_COOKIES` is guaranteed true; keep the old name as
a read-only compatibility fallback if an existing client depends on it. Keep `SameSite=Lax`.

---

## F-16 — Download quota check is a time-of-check/time-of-use race

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-362 (Race Condition) / CWE-367 (TOCTOU) |
| **OWASP** | API4:2023 |
| **Skills** | `hunt-race-condition`, `hunt-business-logic` |
| **Affected** | `internal/downloads/manager.go:284-351`; `migrations/0003_downloads.sql:29-32` |

`Create` counts active jobs for the user, then counts all active jobs, then inserts. Two
concurrent requests with different info hashes can both pass both counts and exceed
`MaxActivePerUser` / `MaxActive`. The same applies to the free-space check and to the
`FindDownloadJobByHash` duplicate check.

The partial unique index `uq_download_jobs_active_hash` prevents two **identical** active
torrents, but the resulting unique violation is wrapped generically and mapped to a 500 by
`writeCreateError` (which has no `IsUniqueViolation` branch), rather than a 409 with the existing
job id.

### Impact

A user with `can_download` can slightly exceed their own quota and consume more disk/bandwidth
than configured; no cross-user impact. The duplicate race produces a server error instead of the
documented 409.

### Remediation

Serialize the quota decision in the database, e.g. a transaction with
`pg_advisory_xact_lock(hashtext(user_id))`, or an `insert ... select ... where (select count(*) ...) < $limit`.
Map `23505` to `DuplicateJobError`. Repeat the check after a conflicting insert.

### Regression test

`REGRESSION_TESTS.md` §19 (`TestConcurrentDownloadCreateRespectsQuota`).

---

## F-17 — `can_download` lookup pages the entire user table

| Field | Value |
|---|---|
| **Severity** | **Informational** |
| **Status** | Confirmed |
| **CWE** | CWE-405 (Asymmetric Resource Consumption) |
| **Skills** | `hunt-business-logic`, `performance` |
| **Affected** | `internal/httpx/handler_downloads.go:59-84` |

`storeEntitlements.CanDownload` calls `ListUsers(ctx, 200, offset)` repeatedly until it finds the
user. Every non-admin download create/delete performs `ceil(N/200)` queries. With many accounts
this is a database amplifier, though the route is rate-limited (10/min create, 60/min write) and
the intended deployment has few users.

### Recommendation

Add a single-row repository method:

```go
func (s *Store) CanDownload(ctx context.Context, userID string) (bool, error) {
    var allowed bool
    err := s.pool.QueryRow(ctx,
        `select can_download from animind_users where id = $1::uuid`, userID).Scan(&allowed)
    if errors.Is(err, pgx.ErrNoRows) { return false, ErrNotFound }
    return allowed, err
}
```

---

## F-18 — CI/CD and image supply-chain hardening gaps

| Field | Value |
|---|---|
| **Severity** | **Low** |
| **Status** | Confirmed |
| **CWE** | CWE-1104 / CWE-1357 (Reliance on Insufficiently Trustworthy Component) |
| **Skills** | `hunt-cicd`, `supply-chain-attack-recon` |
| **Affected** | `.github/workflows/ci.yml:22-29`, `:83-88`, `:116-124`; `Dockerfile:3`, `:14`; `docker-compose.yml:18`, `:89`, `:109`, `:134`, `:157` |

Positive controls already present: `permissions: contents: read`, no repository secrets, no
`pull_request_target`/`workflow_run`, Dependabot for Go/Actions/Docker, tests run with `-race`,
and the API container runs as a non-root user.

Gaps:

- Actions are pinned by tag (`actions/checkout@v4`, `actions/setup-go@v5`) rather than commit SHA.
- No `govulncheck`, SAST, secret scanning or SBOM/signing step in CI.
- Base/runtime images are tag-pinned, not digest-pinned; `alpine:3.20` is at/near the end of its
  supported window (released May 2024, two-year support), so it will stop receiving fixes.
- Compose images (`nginx:1.27-alpine`, `postgres:16-alpine`, `redis:7-alpine`,
  `prom/prometheus:v2.53.0`, `grafana/grafana:11.2.0`) are older tags; the compose file is
  development-only, but operators do copy it.
- `build/` in the working tree contains compiled binaries, `.torrent.db*` and logs (git-ignored,
  not tracked). They can leak local paths/history if the folder is shared. No secret was found
  in the two log files during this audit (only a user id and `method=password` appeared).

### Recommendation

SHA-pin actions, add `govulncheck` + `gosec`/Semgrep + gitleaks to CI, move to a supported Alpine
base and digest-pin it, generate an SBOM, and document the compose file as non-production. Clean
`build/` before sharing the folder.

---

## F-19 — Error details leak outside production

| Field | Value |
|---|---|
| **Severity** | **Informational** (part of F-05) |
| **Status** | Confirmed |
| **CWE** | CWE-209 (Generation of Error Message Containing Sensitive Information) |
| **Skills** | `hunt-exceptional-conditions` |
| **Affected** | `internal/httpx/handler_auth.go:598-610` |

```go
func safeDetail(err error, production bool) string {
    if production || err == nil { return "" }
    return err.Error()
}
```

Any deployment not running with `ENV=production` returns the wrapped error text in
`details`. Examples reachable from unauthenticated routes include repository errors (table/column
names), storage path errors and provider error strings. This is only an information-disclosure
issue, but it assists targeted attacks and is trivial to fix.

### Recommendation

Gate detail on an explicit `DEBUG_ERRORS` flag that defaults false in every environment. Keep the
structured log for operators.

---

## F-20 — Email address is written to the recovery log

| Field | Value |
|---|---|
| **Severity** | **Informational** |
| **Status** | Confirmed |
| **CWE** | CWE-532 (Insertion of Sensitive Information into Log File) |
| **Skills** | `evidence-hygiene`, `hunt-forgot-password` |
| **Affected** | `internal/httpx/handler_auth.go:170-175` |

```go
s.logger.Warn("password reset token issued",
    slog.String("email", auth.NormalizeEmail(req.Email)),
    slog.String("token_hash", auth.HashToken(token)),
    ...)
```

The token hash is not a usable credential, but it is unnecessary, and the email address is PII in
a log sink. If logs are shipped to a third-party service, this creates a privacy-processing
surface for addresses that may not even have accounts (the log line is only emitted when the
account exists, so it also confirms existence to a log reader).

### Recommendation

Log the user id (or a keyed hash of the email) and drop the token hash. If support needs to
correlate a reset, the database row already carries `user_id`, `kind` and `created_at`.

---

## F-21 — Dead lockout/session code can mislead future reviewers

| Field | Value |
|---|---|
| **Severity** | **Informational** |
| **Status** | Confirmed |
| **CWE** | CWE-561 (Dead Code) |
| **Skills** | `code-review` |
| **Affected** | `internal/auth/service.go` (`RegisterFailedLogin` in `repository/users.go:180-198` is unused); `repository/users.go:98-104` (`EmailTaken` unused); `internal/auth/service.go:430-437` (`LogoutEverywhere` unused); `internal/syncplay/hub.go:48-50` (`pendingPeers` only initialized/tested) |

`RegisterFailedLogin`, the `failed_login_count`/`locked_until` columns, `EmailTaken` and
`LogoutEverywhere` are not wired to any route. A reviewer (or a future developer) may believe
account-wide lockout and "log out everywhere" exist because the code and schema say so. The live
lockout is the per-(email,source) limiter (F-06), and logout-everywhere is not exposed.

### Recommendation

Either wire the controls (a `POST /api/auth/logout-all` route is useful, and a per-account
backoff could complement the per-source one) or delete the dead paths and columns so the
security model is legible. Do not leave a control half-present.

---

## F-22 — Web sessions never record client IP or user agent

| Field | Value |
|---|---|
| **Severity** | **Low** (security monitoring / incident response) |
| **Status** | Confirmed |
| **CWE** | CWE-778 (Insufficient Logging) |
| **OWASP** | A09:2021 Security Logging and Monitoring Failures |
| **Skills** | `hunt-session`, `code-review` |
| **Affected** | `internal/httpx/handler_auth.go:43-108`, `:268-354`; `internal/auth/service.go:278-298`; `internal/auth/context.go:17-37`; `internal/httpx/middleware.go:43-46` |

### Root cause

`SignInUser` records the request context's client metadata:

```go
s.store.CreateWebSession(ctx, userID, HashToken(token), deviceName,
    userAgent(ctx), clientIP(ctx), rememberMe, expiresAt)
```

`userAgent`/`clientIP` read the value attached by `auth.WithClient`. That attachment exists only
in `RequireAuth` (and the media `authorizeStream` path):

```go
// middleware.go — RequireAuth only
ctx := auth.WithClient(r.Context(), auth.Client{
    UserAgent: r.UserAgent(),
    IPAddress: ClientIP(r, s.cfg.TrustProxy),
})
```

The routes that actually **create** a web session — `handleSignup`, `handleLogin`,
`handleGoogleCallback`, `handleConsumeCode` — are outside `RequireAuth` and never call
`WithClient`. `handleLogin` attaches only `WithLoginSource` (the rate-limit key). Therefore every
row in `animind_web_sessions` is written with an empty user agent and a `NULL` IP address. The
security screen (`GET /api/auth/sessions`) returns `host(ip_address)` and `user_agent`, both
empty, so a user cannot tell where a session came from; incident response loses the primary
attribution signal.

### Impact

Weak compromise detection and response. A stolen session cannot be distinguished from the
owner's by address or client, and the account owner sees a list of anonymous sessions. No direct
access is granted by the bug itself.

### Remediation

Attach the client once, at the edge, for every route that can create a session:

```go
// handleSignup, handleLogin, handleGoogleCallback, handleConsumeCode
ctx := auth.WithClient(r.Context(), auth.Client{
    UserAgent: r.UserAgent(),
    IPAddress: ClientIP(r, s.cfg.TrustProxy),
})
result, err := s.auth.Login(ctx, req.Email, req.Password) // attach login source too
```

Alternatively, have `SignInUser` accept a `Client` parameter so the compiler forces every caller
to supply it. Keep the values bounded (`userAgent` is already truncated to 512; validate the IP
with `net.ParseIP` before the `inet` cast). Add the IP/user-agent to the session list UI and
consider a "new device" notification.

### Regression test

`REGRESSION_TESTS.md` §20 (`TestWebSessionRecordsClientMetadata`).

---

## 7. Potential findings (not counted)

| ID | Description | Why it is not confirmed | Recommended follow-up |
|---|---|---|---|
| P-01 | Stored XSS through subtitle `content`, watchlist `anime_data`, or history `thumbnail_url` | The frontend that renders these values is not in this repository; the API emits JSON with `nosniff` | Audit the site/desktop renderer for `innerHTML`/`dangerouslySetInnerHTML`; sanitize VTT/SRT text if rendered as markup |
| P-02 | Torrent metainfo path escape during download (before import) | The importer defends after download, but file writes before import are performed by upstream anacrolix storage; the exact upstream version's path handling was not proven unsafe | Add a fuzz test with `../`/absolute/UNC file names; consider a custom storage wrapper that rejects escapes at write time |
| P-03 | ffmpeg/media parser RCE on a malicious file | Inherent to processing untrusted media; no specific vulnerable codec/version was identified | Run ffmpeg in a sandbox/container with no network and a read-only library; keep ffmpeg patched |
| P-04 | Scanner LLM prompt injection mislabels the catalog | The LLM has no tools and no cross-user data; impact is data integrity of the operator's own library | Treat the LLM output as untrusted text; keep the confidence floor; never let it write paths |
| P-05 | Public S3 bucket / weak bucket policy | Bucket policy is deployment state, not in the repository | Verify `s3:GetObject` is not public; use least-privilege keys; enable Block Public Access |
| P-06 | Metrics listener exposed publicly | Default is loopback and compose does not publish it; exposure is a deployment decision | Keep `METRICS_ADDR` on loopback/internal; add auth or mTLS if it must cross a network |

## 8. False positives examined and rejected

| Signal | Why it is not a finding |
|---|---|
| `File`-like path parameters (`{id}`, `{sessionId}`, `{segment}`) | UUID/pattern validation and DB lookups prevent traversal; HLS segment names are regex-validated |
| `?st=` stream ticket in a URL | Scoped to one episode, signed, expiring; no identity or catalog access; documented |
| `GET /api/hls/*` unauthenticated | Session id is 128-bit random, minted only by an authenticated request; segment names are validated |
| JWT `alg` header | Only `HS256` accepted; `none`/key-confusion rejected |
| `Origin` absent on CORS/WS requests | Desktop clients send no Origin; a browser cannot omit it, so CSWSH is not enabled |
| Webhook endpoint without a session | Shared secret with constant-time compare; production rejects the dev default |
| Admin route without `RequireAdmin` | Every `/api/admin/*` and `/api/rescan` route has both middleware |
| `DeleteAccount` without password | Requires a live session; destructive but not a CSRF due to `SameSite=Lax` and DELETE; step-up is a hardening item |
| Duplicate username | Display name only; no authorization decision depends on it (watch-party impersonation is social, not privilege) |
| `anime_data`/`preferences` arbitrary JSON | Stored and returned as JSON with `nosniff`; no server-side execution |
| Password reset response wording | Identical for found/not-found; no token in the response |
| `S3` presigned URL in a redirect | Standard design; TTL is configurable and the URL is a bearer for one object |
| `TRUST_PROXY_HEADERS=true` in compose | nginx overwrites `X-Forwarded-For`; the API port is bound to loopback in compose |
| `ffmpeg -map 0:<index>` with user index | Argument vector, integer, no shell; invalid indexes fail the process |
| `os.RemoveAll` on job dirs | Job id is a UUID and the joined path is checked against the download root; `RemoveAll` does not follow a final symlink |
