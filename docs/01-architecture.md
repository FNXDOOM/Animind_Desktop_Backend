# 1. Architecture

## Shape of the process

One binary, one port, one process. It serves JSON over HTTP, a Socket.IO-compatible socket
for watch parties, and byte ranges for media. There is no queue and no second service: a
personal streaming backend on a single VPS is the deployment this is designed for, and adding
coordination would be the cost, not the benefit.

Shared state is the one exception, and it is optional. `internal/cache` holds resolved
credentials, rate-limit windows and upstream metadata; with no `REDIS_URL` all three live in
this process and behave the same. Redis is what makes the rate-limit window one budget instead
of one per replica. See [Caching and observability](10-caching-and-observability.md).

```
cmd/server/main.go        wiring only: config -> db -> cache -> services -> hub -> http -> signals
  |
  +-- internal/config       every setting, validated at boot
  +-- internal/logging      slog setup, request log fields
  +-- internal/database     pgx pool + embedded migrations
  +-- internal/repository   all SQL, one file per domain
  +-- internal/cache        shared state: Redis or in-process, keys, windows, group eviction
  +-- internal/auth         accounts, hashing, tokens, sessions, OAuth, bridge JWT
  +-- internal/httpx        router, middleware, handlers, metrics (the HTTP edge)
  +-- internal/storage      S3 presign + local paths + library file listing
  +-- internal/media        ffmpeg probing, subtitles, audio variants, HLS sessions
  +-- internal/metadata     AniList + MyAnimeList, cache, rate-limit cooldown
  +-- internal/scanner      filename parser, indexing, pruning, LLM fallback
  +-- internal/torrent      Torznab indexers, search ranking, opaque ids, trending
  +-- internal/downloads    engine adapter, job state machine, quotas, importer, SSE hub
  +-- internal/syncplay     Engine.IO/Socket.IO framing, room state machine
  +-- internal/scheduler    cron jobs with overlap protection
migrations/*.sql           schema, embedded into the binary and readable by psql
deploy/                    nginx, prometheus and grafana configuration for the compose stack
docs/                     this set, plus openapi.yaml
```

Dependency direction is one-way: `httpx` and `scheduler` depend on everything else; the
domain packages depend on `config`, `repository`, and each other only downward. No package
imports `httpx`. `repository` imports nothing from the app except `database`, so SQL cannot
reach into HTTP concerns. `cache` imports only `config`: it knows about keys, windows and
eviction, and nothing about sessions, routes or AniList.

## Request lifecycle

```
Recover -> RequestLogger -> observeRequests -> SecureHeaders -> CORS -> StripSlashes
   -> route match (chi)
   -> per-route rate limiter (keyed by client IP, counted in the shared store)
   -> RequireAuth / RequireAdmin where the route declares them
      -> credential cache, then Postgres on a miss
   -> handler: decode -> authorize by session -> repository -> respond
```

Middleware order is fixed in `Server.Router()` (`internal/httpx/server.go`). Rate limiting
runs *ahead* of authentication so a credential-stuffing loop is bounded by the limiter rather
than by the argon2id cost, which is deliberately expensive.

Two authentication paths exist because one client cannot send a header:

| Path | Credential | Where |
| --- | --- | --- |
| API call | `Authorization: Bearer` or the `animind_session` cookie | `credential()` in `server.go` |
| Media bytes | signed playback ticket in `?st=` | `authorizeStream()` in `handler_media.go` |

The ticket is scoped to one episode, one expiry, and optionally one audio track. It is not an
identity credential: it cannot browse the catalog.

## Data access

`internal/repository` holds every statement. Three rules explain most of its shape:

1. **IDs are strings.** Columns are read with `x::text` and written with `$n::uuid`. This
   keeps JSON marshalling trivial and avoids a driver-level UUID codec, and `isUUID()`
   pre-checks client input so a malformed id answers 404 rather than a Postgres syntax error
   surfacing as 500.
2. **Numeric columns are cast on read.** `rating::float8`, `episode_number::float8` decode
   straight into Go floats; `numeric` in Postgres exists because the original schema had it
   and fractional episode numbers (5.5 for a special) are real.
3. **Ownership lives in the query.** Every user-data statement filters `user_id = $n::uuid`
   with the id taken from the session. There is no client-supplied `user_id` anywhere, which
   is the specific way the previous browser-to-database setup could be subverted.

Migrations are plain SQL files under `migrations/`, embedded with `//go:embed`, applied in
filename order inside a transaction, and recorded in `schema_migrations`. Because they are
also normal files, an operator can apply them with `psql -f` instead.

## Concurrency model

| Resource | Guard | Reason |
| --- | --- | --- |
| HLS sessions | `HLSManager.mu` + per-session `entry.mu` | Each session owns an ffmpeg process; eviction and destroy must not race it |
| Watch rooms | `Hub.mu` (registry) + `Room.mu` (state) | The registry is read on every broadcast; room state changes per event |
| Scanner | `Service.mu` with a `running` flag | Two overlapping scans would double metadata traffic and fight over pruning |
| Scheduler jobs | `Jobs.mu` with a per-job `running` map | A job that outlives its interval is skipped, not stacked |
| Metadata cache | `Service.mu` + in-flight map | A 200-episode scan asks for the same title 200 times concurrently |

There are no data races by design rather than by inspection: shared maps are only touched
under their mutex, and handlers never hold a lock across a network or ffmpeg call. The
`ffmpeg` runs are bounded by `FFMPEG_TIMEOUT_SECONDS`, and every outbound HTTP call has a
timeout.

## Long-running work

Cron jobs (`internal/scheduler`) and background goroutines spawned by handlers are the only
deferred work. Handlers that start something expensive answer first:

- `POST /api/webhooks/storage` replies `{received:true}` then scans.
- `POST /api/rescan` scans synchronously (the client waits for a fresh library list) but
  prewarms audio variants afterwards in the background.
- `GET /api/health` answers `degraded` with 503 when the database is unreachable, so an
  orchestrator can act on it.

Shutdown order in `main.shutdown()`: stop the scheduler, close every watch room (so clients
get a clean disconnect), stop accepting HTTP, then kill every HLS ffmpeg process and remove
its segments. Reversed, orphaned ffmpeg processes would keep temp files open.

## Deliberate limitations

Stated so they are not mistaken for oversights:

- **Rate limits are shared only when Redis is configured.** With no `REDIS_URL` each process
  counts its own window, so multi-instance deployment multiplies each budget.
- **Watch rooms live in memory.** A restart ends every party; the database rows exist for
  lookup and cleanup, and startup closes the stale ones. This is deliberate: party state is
  synchronised to sub-100 ms, and a network hop per gate decision is the wrong tool for it.
- **`GET /api/hls/*` is unauthenticated** by necessity: a player fetches segments without
  headers. Unguessability of the 128-bit session id plus the strict `seg\d{5}\.ts` name
  pattern is the control, and segments live only in this server's temp directory.
- **Subtitle extraction and audio variants need local mode.** With S3 the file is handed to
  the client as a presigned URL, so there is nothing to write next to it.

Related: [Authentication](02-authentication.md), [Storage and scanner](04-storage-and-scanner.md),
[Streaming](05-streaming.md), [Watch parties](06-watch-parties.md).
