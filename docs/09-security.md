# 9. Security

Result of a systematic review against the OWASP patterns, scoped to this service. Findings are
listed with where the input actually comes from; items that look risky but are driven by
server-side configuration are called out as such rather than reported as vulnerabilities.

## Summary

| | |
| --- | --- |
| Threat surface | Unauthenticated catalog + media routes, authenticated user data, admin routes, a stateful socket protocol, and an ffmpeg/object-store backend |
| Design stance | Authorization in SQL by session-scoped predicates; every credential hashed or signed; no third-party identity provider holding the users |
| Findings fixed during this port | 7 |
| Accepted, documented risks | 8 |
| Verification | The `internal/storage` suite covers path containment; `internal/httpx` asserts every protected route refuses anonymous callers; `internal/auth` asserts signature and `alg=none` rejection |

## Attacks and where they are stopped

| Attack | Control | Where |
| --- | --- | --- |
| Credential guessing | argon2id (64 MiB, t=3, p=4) + 20/min login limit + per-account lockout at 10 failures | `auth/password.go`, `httpx/ratelimit.go`, `repository/users.go` |
| Account enumeration | Identical response and comparable timing for unknown email vs wrong password (`burnTime` runs a real hash) | `auth/service.go` |
| Token forgery | HMAC-SHA256 over the payload, compared with `hmac.Equal`; signature checked before the payload is decoded | `auth/tokens.go`, `auth/jwt.go` |
| `alg: none` JWT | Header must decode to `HS256` before anything else | `auth/jwt.go` |
| Token replay across purpose | Fixed `aud` per token type: bridge, OAuth state, and playback tickets are not interchangeable | `auth/jwt.go`, `auth/service.go` |
| Ticket replay for another episode | Ticket payload carries `episodeId`; verification requires it to equal the requested episode | `auth/tokens.go:VerifyStreamTicket` |
| Stolen token reuse | Access and refresh tokens are stored only as SHA-256 hashes and must match a live, unrevoked, unexpired row | `repository/desktopsessions.go` |
| Refresh-token theft | Rotation replaces both hashes in one statement, so a presented refresh token works once | `auth/service.go:DesktopRefresh` |
| Session fixation | Opaque 256-bit session tokens generated server-side; nothing user-chosen becomes a credential | `auth/tokens.go:NewWebSessionToken` |
| IDOR on user data | Every statement filters `user_id` from the session; no endpoint accepts a client-supplied user id | `repository/userdata.go` |
| Account takeover through an email change | `PATCH /api/me` with `email` re-verifies the current password, refuses a Google-only account with `password_not_set`, and revokes every **other** web session so a stolen cookie cannot outlive the change | `auth/service.go:ChangeEmail` |
| Privilege escalation | `RequireAdmin` reads `is_admin` from the account row per request; self-demotion refused | `httpx/middleware.go`, `handler_auth.go` |
| SQL injection | All values are parameters; the only string-built SQL is placeholder numbering (`$1`, `$2`) and constant predicates | `repository/*.go` |
| Path traversal | One choke point resolves and re-checks containment, including symlink resolution | `storage/storage.go:LocalPath` |
| Arbitrary file read via HLS | Session id must match `^[a-f0-9]{32}$`, segment name `^seg\d{5}\.ts$`, and both are resolved under the session directory | `media/hls.go` |
| Command injection | `exec.Command` with argument arrays, never a shell; binary paths come from configuration only | `media/ffmpeg.go`, `media/hls.go` |
| Open redirect after OAuth | `redirect` accepted only when its origin matches `FRONTEND_URL`, or an `APP_LINK_ORIGINS` entry merged in only for this check; app-link entries must be https with no path, query, fragment, userinfo or wildcard | `httpx/handler_auth.go:sanitizeReturnTarget`, `config/config.go:parseAppLinkOrigins` |
| CSRF | Session cookie is `HttpOnly` + `SameSite=Lax`; state-changing routes additionally accept the bearer header, which a cross-site form cannot set | `httpx/server.go:setSessionCookie` |
| SSRF | Every outbound request targets a constant or operator-configured URL (AniList, MyAnimeList, OpenRouter, Google). No client-supplied URL is ever fetched | `metadata/*`, `auth/google.go` |
| GraphQL abuse | Proxy strips comment lines then refuses any document containing `mutation` or `subscription`, capped at 10,000 characters | `httpx/handler_library.go` |
| Response-size abuse | Every upstream body is read through `io.LimitReader` (1 MiB metadata, 8 MiB object reads, 4 MiB proxy) | `metadata/`, `storage/`, `httpx/` |
| Body-size abuse | 1 MiB JSON cap on bodies, 64 KiB for a preferences document, 64 KiB per socket message | `httpx/`, `repository/`, `syncplay/` |
| Slow-line / memory exhaustion in sockets | Read limit, bounded per-peer send channel (packets are dropped for a client that cannot keep up rather than growing a queue), polling sessions expire after 2 idle minutes | `syncplay/protocol.go` |
| Resource exhaustion in scan | One scan at a time (409 on overlap), jobs skipped when the previous pass is still running, sequential subtitle extraction | `scanner/scanner.go`, `scheduler/scheduler.go` |
| Webhook spoofing | `x-webhook-secret` compared in constant time; wrong or missing secret is 401 | `httpx/handler_library.go` |
| Mass assignment | Request structs are explicit with `DisallowUnknownFields`, so an extra `is_admin` in a body cannot reach a update statement | `httpx/responses.go:DecodeJSON` |
| Sensitive data in responses | `file_path`, `bucket_name`, `password_hash`, `token_hash` are tagged `json:"-"` or live on non-serialized fields | `repository/models.go` |
| Internal detail leakage | Errors are `{ "error": generic }`; `details` is populated only outside production; panics log a stack and answer 500 | `httpx/handler_auth.go:safeDetail` |

## Fixed during this port

Each of these existed in the Node backend and is closed here:

1. **Client-supplied `user_id` on every user-data write.** The site and the desktop player wrote
   `watchlist` and `progress` rows directly with an `user_id` they chose. Now: derived from the
   session only.
2. **Unauthenticated HLS session destruction.** `DELETE /api/hls/{sessionId}` needed no credential;
   anyone guessing an id could destroy another viewer's session. Now session-authenticated, and ids
   are 128-bit.
3. **No revocation path for site sessions** other than token expiry. Now session rows carry
   `revoked_at`, `GET /api/auth/sessions` lists them, `DELETE` revokes one, and a password change
   revokes all of them.
4. **Non-constant-time comparison of a shared secret** on the webhook during this review; now
   `subtle.ConstantTimeCompare`.
5. **LIKE wildcards passed through to Postgres**, where a search of `%` became a full table scan;
   now escaped (`repository/library.go:escapeLike`). Values were always parameterized, so this was
   availability, not injection.
6. **Lexical-only path containment**: a symbolic link inside the library resolved outside it after
   the check. `LocalPath` now also resolves links and re-checks.
7. **`POST /api/auth/logout` did not revoke anything** — found by the cache harness rather than by
   reading the handler. The route sits outside `RequireAuth` (logging out with an already-dead
   credential must still succeed), so `PrincipalFromContext` was always nil and the revocation
   branch never ran: the cookie was cleared and the session row stayed live until its own expiry.
   The handler now resolves the presented credential itself and revokes when it verifies.

Plus two client-facing holes that were only reachable through the old architecture: the browser held
a database-capable key (`SUPABASE_ANON_KEY` plus an externally issued JWT), and admin rights lived
in a third party's `publicMetadata` where the browser could not be trusted to read them correctly.

## Accepted and documented risks

Stated plainly, because pretending they are gone would be worse than noting them:

| Risk | Why it stands | Bound on the damage |
| --- | --- | --- |
| Catalog and HLS routes are unauthenticated | Browsing before sign-in is a product requirement; a media element cannot send headers | Catalog exposes titles, artwork and AniList ids — nothing that identifies a user. Segment URLs carry a 128-bit session id, are served `no-store`, live in this server's temp dir, and expire after `HLS_SESSION_TIMEOUT_MINUTES` |
| Rate limits are per-process unless `REDIS_URL` is set | A single VPS does not need a coordinator, and the fallback keeps working when Redis is down | Without Redis, multi-instance deployment multiplies each budget; with Redis, a store that stops answering falls back to local counting and exports `animind_rate_limit_fallbacks_total` |
| A revoked credential can stay valid for up to `CACHE_SESSION_TTL_SECONDS` | The cache is written after the row is read, so a revocation landing inside that window cannot be seen by the in-flight write | Only the account's own session is affected, for one minute, and every revocation path also deletes the entry — so the window needs a coincidence measured in milliseconds. Authorization still happens in SQL on every miss |
| The metrics listener is unauthenticated | A Prometheus poll carries no session, and adding one would put a credential in a scrape config | It binds to `127.0.0.1:3002` by default, is not proxied by nginx, and is not published by compose. It exposes route patterns, timings and library counts — no user data, no secrets |
| Redis holds cached identity | Anything in a store someone can read is readable | Keys carry the SHA-256 of a token, never the token; values are the user id, email, username and admin flag. Treat the Redis address as privileged: it is loopback-published here and has no password in the local stack |
| `423` on a locked account confirms the address exists | The lockout message is needed for a usable sign-in screen | Reveals only that an address is registered *and* was recently brute-forced; the 401 path stays indistinguishable |
| Password-reset delivery is deployment-dependent | No mail transport exists in this service and inventing SMTP was out of scope | The API stores only a hash and never logs the live reset credential. Configure an external recovery integration before enabling the endpoint for users |
| Watch parties live in memory | Persisting live playback state on every heartbeat would cost more than a restart costs users | A restart ends the party; startup closes the stale `active` rows so the cleanup job still converges |

Also worth knowing: `anime_data` (watchlist) is stored and returned verbatim as JSON. This service
does not render it, and React escapes interpolated values, so there is no XSS sink here — but a
consumer that inserts those fields into HTML must sanitize them itself.

## Secrets inventory

| Variable | Exposed to clients? | Consequence of leakage |
| --- | --- | --- |
| `DESKTOP_TOKEN_SIGNING_KEY` | Never | Forge desktop access tokens and playback tickets; rotate to revoke everything |
| `WEBHOOK_SECRET` | Header only | Trigger library scans |
| `DATABASE_URL` (password) | No | Full read/write of accounts and library |
| `S3_SECRET_ACCESS_KEY` | No | Read the media bucket; a read-only key is sufficient |
| `GOOGLE_CLIENT_SECRET` | No | Complete an auth-code exchange for an attacker who also holds a code |

`.gitignore` excludes `.env`, `*.key`, `*.pem`; `.dockerignore` keeps them and the docs out of the
image build context. The container runs as a non-root user with `ProtectSystem=strict` and
`ReadWritePaths` limited to the library and temp directory in the documented systemd unit.

## If you deploy this

1. Generate a fresh `DESKTOP_TOKEN_SIGNING_KEY` (`openssl rand -base64 48`) — the example value is
   public.
2. Set a unique `WEBHOOK_SECRET`. Production startup rejects the documented development default.
3. Keep `TRUST_PROXY_HEADERS=false` unless nginx genuinely overwrites `X-Forwarded-For`; a forged
   header is otherwise a free rate-limit bypass.
4. Set `ENV=production` so `SECURE_COOKIES` defaults on and error `details` stay suppressed.
5. Serve over TLS and pin `PUBLIC_URL` to the real origin, since it is the token issuer and the
   OAuth callback base.
6. Bind the process to loopback or a private interface if a proxy fronts it; the app cannot tell a
   proxy from a client that simply claims to be one.
7. Keep `METRICS_ADDR` on loopback, and do not add a `proxy_pass` for `/metrics`. Confirm it with
   `curl -o /dev/null -sw '%{http_code}' https://api.example/metrics` — the answer must be 404.
8. If you set `REDIS_URL`, put Redis on a private network or give it `requirepass`, and never
   publish 6379. It holds cached identity and every rate-limit window: flush it and the limits
   reset, read it and you can see who is signed in.

## Torrent download surface

The download feature adds its own containment rules, all of which fail closed:

- Every download route is behind `RequireAuth`; create and delete additionally require the
  `can_download` entitlement (admins exempt), which the admin panel toggles per account.
- Job ownership comes from the session on every read and write. Another user's job id answers
  404 exactly like a missing one — the id space cannot be probed.
- Magnets never leave the process. The search wire shape is built by a marshaler that has no
  magnet field, and download creation references the opaque result id only; the id is a random
  22-character value that expires with the search cache. The info hash in a result row cannot
  authenticate anything.
- Torznab is operator configuration and is never fetched from a client-supplied URL; the XML
  decoder is strict and size-bounded, and only `torrent`/`item` elements are consumed.
- Torrent entry paths are validated before any write: Windows device names and overlong
  segments are refused outright, traversal paths and non-media extensions are skipped, the file
  count and per-job size are capped, and free space is checked before the job starts.
- Imports are containment-checked (source inside the job directory, destination inside the
  library) and never overwrite an existing file; symlinks are rejected.
- Cancellation with `deleteFiles=true` removes only the job's own directory and imported files,
  after re-checking containment.
- SSE connections are capped (2 per user, 100 total) and events carry state and rates — never
  magnets or filesystem paths.
- Logs redact `Authorization` headers, `?st=` tickets, magnets and full file paths.

## Verification performed

The controls above were exercised, not merely written. Postgres 16 and ffmpeg were both brought
into the loop, which is the only way the last four items below were findable at all.

| Layer | Result |
| --- | --- |
| `go test ./...` without a database | all packages pass; database suites skip |
| `go test ./...` with Postgres 16 | all packages pass, including 10 SQL suites and the watch-party protocol over real websockets |
| `scripts/e2e_verify.py` against the running container | 80/80 checks |
| `scripts/verify_media.py` against the running container | 25/25 checks |
| `docker build -f Dockerfile .` | image builds and serves, which is how the BusyBox `nice` argument was caught |

The live HTTP run covers: authentication and its failure modes, the catalog with a real scan,
ticket-only media bytes with working range and 416 handling, the desktop pairing chain end to
end (bridge JWT → exchange → API call → rotation → replay refusal → revocation → immediate
rejection), per-account data isolation, cross-account session revocation refused, admin
enforcement, the AniList proxy's read-only refusal, the webhook secret, CORS rejection, and the
deletion cascade.

Worth recording because they resisted everything short of a live run: an unparameterized array
cast that made every show insert fail, a case-sensitive `UNIQUE` where the code assumed
otherwise, `nice` without `-n`, numeric `disposition` flags typed as booleans, and a temp file
name from which ffmpeg could not infer an output format.

## Verification commands

```bash
go vet ./...
go test ./...                                  # unit + HTTP-surface tests
go test ./internal/storage/ -run TestLocalPath # path containment
go test ./internal/auth/   -run Token          # signature, expiry, tamper rejection
POSTGRES_TEST_DATABASE_URL=... go test -count=1 ./...   # adds SQL and socket suites
python3 scripts/e2e_verify.py --base http://localhost:3001
python3 scripts/verify_media.py --base http://localhost:3001
```

Related: [Authentication](02-authentication.md), [Deployment](07-deployment.md).
