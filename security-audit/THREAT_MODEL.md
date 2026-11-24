# Threat Model — Animind Backend (Go)

## 1. Engagement context

This is a **defensive internal audit** of a self-hosted personal streaming backend. The
operator's own `SECURITY.md` defines the intended model:

> personal streaming backend on a single VPS, operated by the person who uses it; not a
> multi-tenant commercial service; not for unrelated third parties or an untrusted network
> segment.

The threat model below respects that framing but does not use it to excuse missing controls.
A control that the code claims to provide (for example, "a password reset evicts stolen
sessions") is evaluated against the claim, not against the deployment's convenience.

## 2. Assets

| Asset | Why it matters | Impact if compromised |
|---|---|---|
| Account credentials (password hashes) | account takeover | High |
| Live session tokens / refresh tokens | persistent account access | High |
| Password-reset tokens | account takeover | High |
| `DESKTOP_TOKEN_SIGNING_KEY` | forge desktop access tokens and stream tickets | High |
| `WEBHOOK_SECRET` | trigger library scans | Low (availability) |
| S3 credentials | read/write media bucket | High |
| User data (email, IP, watch history, preferences) | privacy | Medium |
| Media library | confidentiality and integrity of the operator's content | Medium |
| Server process / host | availability and potentially RCE via ffmpeg/torrent parser | High |
| Database / Redis | full data compromise | High |

## 3. Actors and capabilities

| Actor | Capability assumed |
|---|---|
| **A1 — Anonymous internet** | can reach nginx/API, send arbitrary HTTP/WebSocket traffic, register if `ALLOW_SIGNUP=true` |
| **A2 — Registered user** | valid `as_` session; can browse, stream, use watch parties, write own user data |
| **A3 — Download-entitled user** | A2 plus `can_download`; can search Torznab, add magnets, select files, import into the library |
| **A4 — Administrator** | A3 plus user administration, scan, cleanup, metrics view; bootstrap is manual SQL |
| **A5 — Token thief** | holds a stolen web cookie, desktop access token, desktop refresh token or bridge JWT |
| **A6 — Malicious torrent / indexer** | controls file names, metainfo, piece content, tracker responses |
| **A7 — Malicious upstream text** | controls scanner filenames that reach the optional LLM, or AniList response content |
| **A8 — Local unprivileged user** | can read/write the library mount, temp dir, or logs if permissions allow |
| **A9 — Network attacker** | can intercept plaintext HTTP or downgrade it when `Secure` cookies are absent |
| **A10 — Supply-chain attacker** | compromises a Go module, GitHub Action, base image or container registry |

Out of scope: a malicious server operator (they own the data), physical access to the host,
and multi-tenant isolation between mutually hostile customers (explicitly not a design goal).

## 4. Trust boundaries and abuse cases

### 4.1 Authentication boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| Credential stuffing | per-IP login limiter (20/min) + per-(email,source) failure limiter (10/15 min) | distributed botnet defeats per-IP limits; no MFA or breached-password check |
| User enumeration via login | identical 401 for unknown email and wrong password; dummy argon2 for unknown email | **F-06:** lockout returns 423 only for existing accounts; signup returns 409 |
| User enumeration via reset | identical 202 for every address | none observed |
| Password brute force | argon2id 64 MiB, t=3; 8-char minimum | no MFA; 8 chars is the NIST floor, not a strong policy |
| Session prediction | 32 random bytes (web), 48 (refresh), 16 (session id) | none observed |
| Session fixation | server generates the token on login; no client-supplied session id is trusted | cookie has no `__Host-` prefix (F-17) |
| Token replay | web/desktop tokens are bearer credentials; TLS is assumed at the edge | a leaked token is usable until revoked/expired |
| Password reset abuse | 32-byte token, SHA-256 at rest, single-use DELETE, 1 h expiry, all web sessions revoked | **F-01:** desktop sessions survive; reset delivery is not implemented |
| OAuth CSRF / login CSRF | signed single-use state + browser nonce cookie + redirect allowlist | a same-site subdomain could overwrite the state cookie (cookie tossing) |
| Pre-account takeover | Google email must be verified by Google | **F-04:** signup email is never verified, so an attacker can squat a victim's address first |
| Session attribution | session rows carry IP/user agent for the security screen | **F-22:** `WithClient` is not attached on signup/login/OAuth, so both columns are empty |

### 4.2 Authorization boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| Read another user's watchlist/progress/history | SQL `where user_id = $sessionUser` | none observed |
| Read/modify another user's download job | ownership predicate in handler and manager; 404 for foreign ids | none observed |
| Revoke another user's session | `RevokeWebSession`/`RevokeDesktopSessionByID` filter by user id | none observed |
| Reach admin functions | `RequireAuth` + `RequireAdmin` on every `/api/admin/*` and `/api/rescan` | none observed |
| Destroy another viewer's HLS session | 128-bit unguessable id, but no owner binding | **F-08:** a leaked session id lets any authenticated user seek/destroy it |
| Join a private watch party | 5-character room code; authenticated CONNECT; room code is the only secret | **F-09:** no join throttling; participant can control playback |
| Bypass download entitlement | entitlements checked on create/delete; admins exempt | quota TOCTOU (**F-16**) |

### 4.3 Injection boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| SQL injection | all values are pgx parameters; `fmt.Sprintf` only builds placeholder **indices** and constant clause shapes | none observed; `escapeLike` escapes `% _ \` |
| Command injection | `exec.Command` argument slices; no shell; ffmpeg stream indexes are integers | a malicious media file can still exploit ffmpeg itself (parser/RCE class) |
| Path traversal | `storage.LocalPath` lexical + symlink check; HLS segment regex; torrent importer escape checks | symlink TOCTOU only for a local writer (A8) |
| SSRF | no user-controlled URL reaches an HTTP client; AniList/MAL/Google/OpenRouter/Torznab endpoints are config-pinned | Torznab itself can redirect; the configured indexer is trusted |
| XXE | Torznab XML is parsed with Go `encoding/xml`, which does not resolve external entities | none observed |
| Deserialisation | JSON only; no `gob`, `pickle`, `unserialize`, `BinaryFormatter`, Java serialisation | none observed |
| SSTI | no server-side template engine renders user input | none observed |
| Header injection | no user input is written into response headers except numeric range values and validated URLs | none observed |
| Log injection | structured `slog`; user agent truncated; path logged, not query | attacker-controlled email is logged (F-20) |

### 4.4 Browser boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| CSRF on state change | cookies are `SameSite=Lax`; mutations are POST/PUT/PATCH/DELETE with JSON; no state-changing GET | a sibling-subdomain cookie-toss could confuse sessions; no explicit CSRF tokens |
| CORS data theft | credentials only returned for an exact allowed origin; `*` rejected in production | **F-12:** wildcard in non-production reflects any origin with credentials (Lax limits impact) |
| Clickjacking | `X-Frame-Options: DENY` | API responses only; no HTML UI served |
| Open redirect | OAuth return target must match a configured origin | none observed |
| XSS | API returns JSON with `nosniff`; no HTML rendering | subtitle/watchlist/history content is rendered by clients outside this repo; stored-XSS conditional |
| WebSocket CSWSH | `CheckOrigin` rejects disallowed origins; browser always sends Origin | no rate limit/cap on the upgrade (F-02) |

### 4.5 File and media boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| Torrent writes outside its job directory | anacrolix per-job storage + importer symlink check + selection escape check | upstream anacrolix path handling is trusted; a malicious metainfo is a parser attack surface |
| Malicious filename becomes a library path | `sanitizeTitle`, Windows device-name rejection, segment/path length caps | none observed |
| Malicious subtitle becomes stored XSS | content is returned as JSON text; client rendering unknown | conditional on frontend (not in repo) |
| HLS segment path traversal | `^seg\d{5}\.ts$` regex, session id `^[a-f0-9]{32}$` | none observed |
| Disk exhaustion via downloads | per-job size, global/per-user active count, min-free-bytes check | quota TOCTOU (F-16); disk check is advisory |
| ffmpeg resource exhaustion | 2 concurrent HLS sessions, 120 min idle timeout, 120 s process timeout | authenticated user can evict other viewers; seek spawns ffmpeg repeatedly |

### 4.6 Availability boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| Login/signup flood | per-IP limiters | distributed flood; no WAF in compose |
| AniList proxy abuse | 120/min/IP, 1 MiB request cap, 4 MiB response cap | **F-03:** unbounded cache bytes |
| Socket flood | none | **F-02:** unbounded pre-auth peers |
| Slow media clients | 600 req/min/IP; file descriptors held while streaming | no write timeout; many stalled streams can exhaust FDs |
| HLS/ffmpeg flood | 2-session global cap | authenticated eviction/CPU abuse |
| Scan flood | 3/min for `/api/rescan`, 30/min webhook | webhook secret is the gate |

### 4.7 Supply-chain boundary

| Abuse case | Control | Residual risk |
|---|---|---|
| Compromised Go module | `go.sum` checksums, Dependabot weekly | no `govulncheck` in CI; module proxy trust |
| Compromised GitHub Action | `permissions: contents: read`, no repository secrets in workflows | actions pinned by tag, not SHA (**F-18**) |
| Compromised base image | Dockerfile `golang:1.27-alpine`, `alpine:3.20`, compose images | Alpine 3.20 is at/near end of support; images are tag-pinned, not digest-pinned |
| Compromised torrent/indexer | user chooses the result; download entitlement required | torrent content is untrusted by nature |

## 5. Attack trees for the highest-impact goals

### Goal A — Persistent account takeover

```
Steal a web session cookie (XSS in an unverified frontend, log, shared device)
  └─▶ POST /api/auth/desktop/exchange with the cookie
        └─▶ receive 30-day rotating desktop refresh token
              ├─▶ victim changes password / resets password  → desktop row survives (F-01)
              ├─▶ refresh every <30 days                    → access survives indefinitely (F-11)
              └─▶ victim logs out                           → desktop row survives
```
Mitigation: revoke desktop rows on password/email change and reset; add absolute refresh
lifetime + reuse detection; require recent re-auth for desktop exchange after a password change.

### Goal B — Pre-account takeover of a Google user

```
Victim has not registered yet
  └─▶ attacker registers victim@example.com with a password (ALLOW_SIGNUP=true, no verification)
        └─▶ victim clicks "Continue with Google" (Google says email_verified=true)
              └─▶ SignInWithGoogle finds the existing row by email and links the Google identity
                    └─▶ victim now uses an account whose password the attacker set
```
Mitigation: verify signup emails before the account can be used; require password proof or a
confirmation email before linking a provider to an existing password account.

### Goal C — Unauthenticated denial of service

```
Open N WebSocket connections to /api/socket.io (no auth required, no rate limit)
  └─▶ each holds goroutines, buffers and a file descriptor; answer pings to hold forever
        └─▶ process reaches FD/memory limits; nginx worker connections exhausted
```
or
```
POST /api/anilist repeatedly with distinct valid queries
  └─▶ each successful response is cached for 12 h (up to 4 MiB)
        └─▶ local cache reaches its 50 000-entry cap before byte eviction → RSS grows to GBs
```
Mitigation: pre-auth connection cap + per-IP upgrade limiter + auth timeout; cache byte budget
or a much smaller per-document cap.

### Goal D — Forge a session with a known deployment secret

```
Production deployed from the sample environment with ENV unset/development
  └─▶ DESKTOP_TOKEN_SIGNING_KEY = the documented placeholder (33 chars, passes the 32-char check)
        └─▶ mint adk_ tokens for any user id (the DB row check still gates, but bridge JWTs are stateless)
              └─▶ POST /api/auth/desktop/exchange with a forged bridge JWT → desktop pair
```
Mitigation: reject known placeholder values in every environment; require an explicit
`ENV=production` before serving traffic when `PUBLIC_URL` is set; generate secrets at install.

## 6. Assumptions that could invalidate this model

1. TLS termination exists in front of nginx. The repository's nginx listens on port 80.
2. `TRUST_PROXY_HEADERS=true` is only set behind a proxy that **overwrites** `X-Forwarded-For`
   (the bundled nginx does; any other proxy must be verified).
3. The database and Redis are not publicly reachable.
4. The metrics listener is loopback/internal only.
5. The website/desktop clients render untrusted text safely (not verifiable here).
6. The operator does not enable `ALLOW_SIGNUP` for an untrusted population without email
   verification and recovery delivery.
7. The `downloaded` content is not automatically executed; only ffmpeg/parsers touch it.

Each assumption is a deployment control, not a code guarantee. The hardening checklist in
`SECURITY.md` already covers most of them; `INFRASTRUCTURE_AUDIT.md` records the gaps between
that checklist and the shipped compose/nginx defaults.
