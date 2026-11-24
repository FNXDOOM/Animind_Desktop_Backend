# Project Architecture — Animind Backend (Go)

## 1. Repository shape

| Path | Responsibility |
|---|---|
| `cmd/server` | Process entrypoint: config, database, migrations, service wiring, graceful shutdown |
| `cmd/animind-seed` | Development fixture loader |
| `internal/httpx` | HTTP edge: chi router, middleware, handlers, rate limiting, metrics listener |
| `internal/auth` | Accounts, argon2id, web/desktop tokens, JWT/stream tickets, OAuth, sessions |
| `internal/repository` | Every SQL statement; ownership scoping lives in the WHERE clauses |
| `internal/database` | pgx pool creation and embedded migration runner |
| `internal/cache` | Redis or in-process KV, fixed-window limiter, credential/metadata caches |
| `internal/storage` | S3 or local-filesystem media access, presigning, traversal-safe path resolver |
| `internal/media` | ffmpeg/ffprobe wrappers, HLS sessions, audio variants, subtitles |
| `internal/scanner` | Library indexing, filename parser, optional OpenRouter fallback |
| `internal/metadata` | AniList / MyAnimeList clients and title cache |
| `internal/torrent` | Torznab indexer client, search merge/ranking, result store, trending |
| `internal/downloads` | Download job state machine, anacrolix torrent engine, file selection, import |
| `internal/syncplay` | Socket.IO/Engine.IO watch-party hub, rooms, scheduling, persistence |
| `internal/scheduler` | Cron jobs (scan, cache cleanup, session cleanup, party cleanup) |
| `migrations` | Embedded SQL schema (0001 core, 0002 watch parties, 0003 downloads) |
| `deploy` | nginx, Prometheus and Grafana configuration |
| `Dockerfile` / `docker-compose.yml` | Runtime image and local stack |
| `.github/workflows/ci.yml` | Format/build/vet/test/docker CI |
| `scripts` | Python end-to-end verification helpers (local only) |

Language/framework: **Go 1.27.4**, chi v5, pgx v5, gorilla/websocket (Socket.IO-compatible
hand-rolled protocol), anacrolix/torrent, AWS SDK v2, Prometheus client, Redis client.
Database: **PostgreSQL 14+** (16 in compose). Cache: optional **Redis 7**; otherwise in-process.
Storage: **S3-compatible** presigned URLs or **local filesystem** with range streaming and HLS.

## 2. Runtime topology

```
                       Internet / LAN
                             │
                     TLS terminator (not in repo)
                             │
                    ┌────────▼────────┐
                    │  nginx :8080    │  deploy/nginx/nginx.conf
                    │  overwrites XFF │  no TLS listener in this file
                    └────────┬────────┘
                             │ HTTP (compose network)
                    ┌────────▼────────┐        ┌─────────────────────┐
                    │  API :3001      │───────▶│ PostgreSQL :5432    │
                    │  chi + sockets  │        │ users, sessions,    │
                    │  (non-root)     │        │ catalog, jobs       │
                    └───┬────────┬────┘        └─────────────────────┘
                        │        │
          ┌─────────────▼──┐  ┌──▼─────────────┐
          │ Redis :6379    │  │ S3 / library   │
          │ optional cache │  │ media + HLS    │
          └────────────────┘  └────────────────┘
                        │
          ┌─────────────▼──────────────────────────────────────────┐
          │ outbound: AniList, MyAnimeList, Google OAuth,          │
          │ OpenRouter, Torznab indexer, BitTorrent swarm          │
          └────────────────────────────────────────────────────────┘
```

The **metrics listener** is a second `http.Server` on `METRICS_ADDR` (default `127.0.0.1:3002`).
It is intentionally unauthenticated and is not part of the chi router.

## 3. Trust boundaries

| Boundary | What crosses it | Control |
|---|---|---|
| Browser/desktop → API | credentials, cookies, bearer tokens, stream tickets, Socket.IO frames | TLS assumed at the edge; HttpOnly/Lax cookie; bearer + ticket verification; Origin check on socket upgrade |
| API → PostgreSQL | SQL, hashed tokens, user data | parameterised queries; least-privilege is deployment-owned (single DB role in compose) |
| API → Redis | cached principals, rate-limit windows, AniList documents | namespaced keys; credentials cached by token hash; short TTL |
| API → S3 | presigned GET URLs, object keys | bucket credentials from env; presign TTL 4 h default |
| API → local filesystem | library reads, subtitle/audio writes, HLS temp, torrent staging | `LocalPath` containment + symlink re-check; fixed cache dirs; download importer guards |
| API → ffmpeg | file paths and integer stream indexes | argument slices, no shell; source paths come from DB/`LocalPath` |
| API → outbound HTTP | AniList, MAL, Google, OpenRouter, Torznab | endpoints fixed by configuration; no user-supplied URL reaches an HTTP client |
| Torrent swarm → API | metainfo, file names, file contents | metadata timeout, path validation, selection allowlist, size/disk quotas |
| Socket.IO client → hub | auth token, room code, playback commands | token verified in CONNECT; per-message participant check; room code is the only secret |
| Admin → API | user administration, scan, cleanup | `RequireAuth` + `RequireAdmin`; self-demote guard |

## 4. Authentication and session model

### Web (site)
1. `POST /api/auth/signup` or `/api/auth/login` verifies credentials and calls `SignInUser`.
2. `NewWebSessionToken()` creates `as_` + 32 random bytes. Only `SHA-256(token)` is stored in
   `animind_web_sessions`.
3. The token is returned in the JSON body **and** set as the `animind_session` cookie
   (`HttpOnly`, `SameSite=Lax`, `Secure` when configured, sliding expiry).
4. `Authenticate` routes by prefix (`as_`, `adk_`, `adr_`) and resolves web sessions from the
   database (or a ≤60 s credential cache keyed by token hash).

### Desktop
1. The browser calls `POST /api/auth/desktop/bridge-token`, which mints a 60-second HS256 JWT
   (`aud=animind-desktop-bridge`).
2. The player exchanges it (or a web session) at `/api/auth/desktop/exchange` for an `adk_`
   access token and an `adr_` refresh token.
3. Both halves are pinned in `desktop_auth_sessions` as hashes. Access tokens are also
   self-contained (`v1.user.session.exp`) and HMAC-signed, then checked against the row.
4. `POST /api/auth/desktop/refresh` rotates both hashes; the old refresh token stops working.

### OAuth (Google)
1. `/api/auth/google/url` or `/google/start` creates a signed state token
   (`aud=animind-oauth-state`, 10 min) and stores it in the cache for single-use consumption.
   A random browser nonce is bound to the state and stored in an `animind_google_state` cookie.
2. The callback verifies signature + audience + issuer + nonce + single-use state, exchanges the
   code with Google, requires a verified email, and finds/creates/links the account.
3. It then redirects to an allowlisted origin with a 2-minute single-use `auth_code`, or returns
   the session JSON directly when no frontend origin is configured.

### Playback
- `GET /api/episodes/{id}/stream-ticket` requires a session and returns a signed ticket scoped to
  one episode, an optional audio track, a client mode and an expiry (4 h default).
- `GET /api/episodes/{id}/stream` accepts either the ticket or a normal credential.
- HLS session IDs are 16 random bytes (32 hex chars). Playlist/segment GETs are unauthenticated
  by necessity; seek/heartbeat/destroy require a session.

### Watch parties
- Socket.IO/Engine.IO handshake is unauthenticated; the `CONNECT` packet carries the token and
  is verified by the same `auth.Service`. Application events before `CONNECT` are ignored.
- Room codes are 5 characters from a 32-character alphabet. Joining the room makes the peer a
  participant, which allows `play`/`pause`/`seek`/`changeEpisode` for the room.

## 5. Authorization model

- **Object-level:** enforced in SQL. `watchlist`, `progress`, `watch_history`,
  `user_preferences`, `download_jobs` and `animind_web_sessions` statements always include the
  authenticated `user_id`. `download_jobs` handlers additionally re-check ownership and return
  404 for a foreign row.
- **Function-level:** `/api/admin/*` and `/api/rescan` use `RequireAuth` + `RequireAdmin`.
  Download creation/deletion additionally checks the `can_download` entitlement for non-admins.
- **Session revocation:** `Logout` revokes the calling web session; `RevokeSession` accepts a
  web-session row id or a desktop-session id scoped by user; `ChangePassword`, `ResetPassword`
  and `ChangeEmail` revoke all **web** sessions (but not desktop sessions — F-01).
- **Playback:** any authenticated account may stream any catalog episode. There is no per-user
  content entitlement; that matches the documented single-operator design.

## 6. Key state machines and concurrency

| Flow | Mechanism |
|---|---|
| Download job | `queued → fetching_metadata → awaiting_selection → downloading → verifying → importing → ready`, with `paused`/`failed`/`cancelled`; every transition is a CAS on the current state (`TransitionDownloadJob(from,to)`) |
| Download quota | count active jobs per user/global, then insert; database partial unique index on `(user_id, info_hash)` for active states |
| Import | selection allowlist → source symlink check → show-title sanitisation → non-overwriting move |
| HLS | LRU eviction above `MaxConcurrentSessions`, idle sweep every 5 min, one ffmpeg process per session |
| Watch party | room mutex serialises membership and playback; buffer gate with per-peer timers; host promotion on leave |
| Rate limits | fixed window in Redis or local map, 18 named buckets, keyed by client IP |
| Background work | cron scheduler for scans/cleanup; per-download drive goroutines; webhook-triggered scan |

## 7. Data classification

| Class | Examples | Storage |
|---|---|---|
| Authentication secrets | password hashes, session hashes, desktop token hashes, reset-token hashes | PostgreSQL |
| Bearer secrets (live) | session tokens, desktop access/refresh tokens, stream tickets, bridge JWTs | client-held only; server holds hashes |
| PII | email, username, avatar URL, IP, user agent, device labels | PostgreSQL; IP/user-agent visible only to the owning account |
| User content | watchlist JSON, progress, history, preferences | PostgreSQL, scoped by user |
| Media | video files, subtitles, audio variants, HLS segments | library / S3 / temp |
| Operational | metrics, logs, job state | Prometheus listener / process log / PostgreSQL |

## 8. Components not present in this repository

- Website/SPA (frontend XSS sinks are therefore unverifiable here).
- Desktop Qt player and any mobile client.
- TLS terminator / WAF / CDN configuration beyond the bundled nginx HTTP server.
- Mail transport for password reset (documented as an external integration).
- Cloud IAM policy for the S3 bucket; only the client code is present.

These omissions bound the audit's coverage and are reflected in `AUDIT_CHECKLIST.md`.
