# AniMind Backend (Go)

The API for AniMind: a personal anime streaming service with a desktop player
(Qt/QML + libmpv) and a website. This process indexes a media library, authorizes playback,
stores per-user viewing data, runs watch parties, and exposes administration.

It is a rewrite of the original Node/Express backend (`animind-backend`) in Go, with the
authentication system built in rather than delegated to Clerk. It serves **no frontend**:
there is no browser code, no template rendering, and no static asset route in this
repository.

```
Library on disk or in a bucket  ->  scanner  ->  Postgres  ->  this API  ->  desktop player
                                                                        ->  website
                                                                        ->  watch parties (Socket.IO)
```

---

## Quickstart

Requirements: Go 1.27+, Postgres 14+, and ffmpeg/ffprobe when `STORAGE_MODE=local`.

```bash
cp .env.example .env          # then edit: DATABASE_URL, DESKTOP_TOKEN_SIGNING_KEY, secrets
make build                    # -> build/animind-server
make run                       # applies migrations, then listens on :3001
curl localhost:3001/health
```

Docker, which brings its own Postgres:

```bash
docker compose up --build
```

Two operational flags are worth knowing:

| Command | Effect |
| --- | --- |
| `animind-server --config` | Print the resolved configuration summary and exit. |
| `animind-server --migrate-only` | Apply pending migrations and exit. |

The server **fails fast**: a missing `DATABASE_URL`, a short
`DESKTOP_TOKEN_SIGNING_KEY`, an unknown `STORAGE_MODE`, or an unreadable
`LOCAL_STORAGE_PATH` each abort startup with the variable name in the message. It never
starts half-configured and then 500s on first use.

---

## What this backend does

| Area | Behaviour | Document |
| --- | --- | --- |
| Accounts | Email/password with argon2id, Google OAuth, sessions, lockouts, password reset | [Authentication](docs/02-authentication.md) |
| Desktop pairing | `adk_`/`adr_` token pair, rotation, revocation, device labels | [Client integration](docs/08-client-integration.md) |
| Catalog | Shows and episodes from the indexed library, public read | [API reference](docs/03-api-reference.md) |
| Library index | S3 or filesystem scan, release-name parsing, AniList/MyAnimeList enrichment, subtitle extraction, deletion pruning | [Storage and scanner](docs/04-storage-and-scanner.md) |
| Playback | Signed tickets, byte-range passthrough, presigned redirects, browser audio variants, on-demand HLS | [Streaming](docs/05-streaming.md) |
| Watch parties | Socket.IO v4 rooms: server-authoritative clock, scheduled group starts, drift ladder, per-member latency and wait policy | [Watch parties](docs/06-watch-parties.md) |
| Operations | Cron jobs, metrics, webhooks, Docker/VPS deployment | [Deployment](docs/07-deployment.md) |

Package layout and request lifecycle are in [Architecture](docs/01-architecture.md).
Machine-readable endpoint contracts are in [`docs/openapi.yaml`](docs/openapi.yaml).

---

## Configuration

Everything comes from the environment. [`.env.example`](.env.example) documents every
variable with its default and what breaks if it is wrong. The three that matter most:

| Variable | Notes |
| --- | --- |
| `DATABASE_URL` | Required. Any Postgres 14+, including a Supabase Postgres endpoint. |
| `DESKTOP_TOKEN_SIGNING_KEY` | Required, 32+ chars. Signs desktop tokens **and** playback tickets. Rotate it and every desktop session and outstanding ticket dies. |
| `STORAGE_MODE` | `s3` (presigned URLs) or `local` (this process streams bytes and runs ffmpeg). |

Two switches are security-relevant rather than cosmetic:

- `TRUST_PROXY_HEADERS=false` by default. Turning it on without a proxy that overwrites
  `X-Forwarded-For` hands every client a forged rate-limit key.
- `SECURE_COOKIES` defaults to on in production. Turning it off while serving HTTPS exposes
  session cookies to downgrade attacks.

---

## Differences from the Node backend

The wire contract that shipped clients depend on was preserved; the parts that only existed
because of Clerk or because the browser held a database key were replaced. Details and the
client-side changes required are in
[Client integration](docs/08-client-integration.md).

**Kept identical** (so the existing desktop player and site keep working):

- Every pre-existing route path, method, and JSON field name, including the show/episode
  snake_case columns and the `Anime`-shaped rescan response.
- Desktop token format (`adk_` HMAC payload + `adr_` refresh), the exchange/refresh/revoke
  endpoints, and their snake_case response fields.
- Playback ticket format and the `?st=` / `?at=` stream URL shape.
- Socket.IO path `/api/socket.io`, event names, and payload keys.
- `WEBHOOK_SECRET` header auth on `POST /api/webhooks/storage`, and `x-webhook-secret` in
  the CORS allow-list.

**Replaced**:

- Clerk is gone. Accounts, password hashing, sessions, Google OAuth and admin flags live in
  this backend's Postgres. `DESKTOP_TOKEN_SIGNING_KEY` is now the only auth secret.
- Supabase's hosted auth and client-side PostgREST writes are gone. Watchlist, progress,
  continue-watching and preferences are authenticated endpoints under `/api/me/*`, because
  there is no longer an externally verifiable JWT for the browser to present.
- `POST /api/auth/desktop/exchange` accepts this backend's own credential instead of a Clerk
  JWT; the site mints a short-lived bridge JWT for the desktop handoff.
- Supabase JS/PostgREST was replaced with `pgx` and explicit SQL, so queries are visible and
  indexable rather than generated at request time.

**Fixed while porting** (each noted in the linked document):

- Watchlist/progress queries no longer accept a client-supplied `user_id`; ownership comes
  from the session.
- `DELETE /api/hls/{sessionId}` now requires a session; previously any guessed id could be
  destroyed.
- A peer that joins a second room leaves the first cleanly. The original leaked a stale
  participant into the old room, which could block its ready gate forever.
- HLS sessions have a `POST /api/hls/{sessionId}/heartbeat` keepalive; before, a long buffer
  gap could expire the session with no way to hold it open.
- The AniList proxy rejects mutations and subscriptions after stripping comments.
- `GET /api/admin/metrics` reports real numbers; the site's dashboard values were hard-coded.
- Library listing gained `q` and `genre` parameters, and episode rows carry a `thumbnail`.

**Watch-party protocol, second pass** (2026-10-01, additive — every field name was chosen to match
what the shipped web client already parses, so the site gained the behaviour without a release):

- `scheduledPlayAt` on play/unpause/seek, computed from the slowest member's ping. A group no
  longer starts when each peer happened to receive the packet.
- `seq` and `serverTime` on every broadcast, so a stale pause that overtook the play it followed is
  detectable instead of mysterious.
- `serverReceiveTime` on `timesync_pong`. With two stamps, latency and clock error are one number;
  with four, a client can tell a slow link from a wrong wall clock.
- `allReady` and `peerBuffering`, which the web client had been listening for since it was written.
- `syncConfig` and a `features` list in the create/join acks, so thresholds are redeployed rather
  than released.
- `ignoreWait` per member, a 30 s wait deadline, an explicit `groupState`, a buffer goal scaled to
  the episode remaining, and corrections that name the clock they were measured against.

Two defects surfaced only in the tests written for that work: the socket writer dropped queued
frames when a peer closed, so a refused handshake reached the client as an unexplained drop rather
than a reason; and three room paths read state after unlocking.

---

## Development

```bash
make test        # unit tests; Postgres integration tests skip without a database
make test-db     # full suite against POSTGRES_TEST_DATABASE_URL
make vet cover
```

Test coverage is deliberate about what it proves:

| Suite | What it pins |
| --- | --- |
| `internal/auth` | argon2id encode/verify, parameter upgrade, token tampering, `alg=none` refusal, ticket episode binding |
| `internal/httpx` | every route is registered, every protected route rejects anonymous callers, CORS, security headers, rate limits, range parsing, and a websocket upgrade that survives the middleware chain |
| `internal/scanner` | release-filename rules and their ordering, candidate scoring, metadata fill |
| `internal/media` | ASS/SRT to WebVTT, ffprobe JSON decoding, codec and container capability split, subtitle directory layout |
| `internal/metadata` | provider field mapping and title normalization |
| `internal/config` | defaults, required-value failures, clamps, boolean parsing |
| `internal/storage` | path containment including symlink escape, discovery skipping the derived cache, atomic writes |
| `internal/syncplay` | Socket.IO framing and ack ids, batched packets, the drift ladder, room codes, the gate machine with no database (`gate_test.go`), and the protocol over real websockets |
| `internal/repository` | SQL against real Postgres: unique keys the upserts target, cascade deletes, ownership filters, session rotation |

The repository suite and `TestWatchParty*` are the only ones that need a database. The
watch-party gate machine is also covered without one, by building a hub whose peers own in-memory
outbound channels — a protocol whose only test skips when a service is absent is a protocol with
no test on the machine doing the deploy. It skips cleanly when `POSTGRES_TEST_DATABASE_URL` is
unset:

```bash
docker compose up -d db
POSTGRES_TEST_DATABASE_URL=postgres://animind:animind@localhost:5432/animind?sslmode=disable \
  make test-db
```

Adding a route or changing a payload is expected to come with a test in the matching suite,
a `docs/openapi.yaml` entry, and a check in the live harness below.

| Layer | What it proves | Needs |
| --- | --- | --- |
| Go unit tests (`make test`) | parsing, token formats, framing, ranges, config, HTTP surface | nothing |
| Go database tests (`make test-db`) | the SQL itself, and the watch-party protocol over real websockets | a Postgres |
| Live harnesses (`scripts/`) | running-server behaviour: status codes, headers, byte ranges, scanner + ffmpeg, HLS, variants | a running server, ffmpeg for the media one |

```bash
python3 scripts/e2e_verify.py --base http://localhost:3001   # auth, catalog, playback, pairing, admin
python3 scripts/verify_media.py --base http://localhost:3001 # subtitle extraction, variants, HLS
```

`verify_media.py` needs a library with a real Matroska file carrying an ASS subtitle track and
two audio streams, because its subject is what ffprobe and ffmpeg report about one.

---

## What has been run, not just written

The port was verified against a live stack rather than only compiled: Postgres 16 in Docker,
the API built from `Dockerfile` and run with the library mounted, both harnesses executed
(80/80 HTTP checks, 25/25 media checks), and the Go suite run with the database configured.

The scheduled start was then measured against that stack with two peers, one holding its
broadcasts back 400 ms: both fired on the same hub instant, **0 ms apart**, with no corrective
seek — the impaired peer's own estimate read the delay as `pingMs=400`.

That verification found seven defects that type-checking and unit tests did not:

| Found by | Defect | Effect had it shipped |
| --- | --- | --- |
| database suite | `coalesce($4, '{}')` left the genres parameter typed as text | every show insert fails, so the scanner can never index a library |
| database suite | `shows.title UNIQUE` is case-sensitive | `Frieren` and `FRIEREN` become two shows with half the episodes each |
| media suite on Linux | `nice 19 ffprobe` without `-n` | BusyBox reads `19` as the program: every probe and transcode fails in the container while working on a GNU host |
| media suite on Linux | ffprobe reports `disposition` flags as numbers | probe JSON never decodes: audio-track lists come back empty and subtitles are never extracted |
| media suite on Linux | a temp variant named `…mp4.partial` | ffmpeg cannot infer a muxer from it, so every audio-variant transcode fails and browsers get the unplayable original |
| middleware socket test | queued frames dropped when a peer closed | a refused sign-in token arrived as a silent disconnect, so the player reported "connection lost" instead of "token expired" |
| gate unit test | three room paths read state after unlocking | a heartbeat racing a gate close could publish a roster that never existed |

The lesson is worth stating: a host with no database and no ffmpeg can prove the build, not the
behaviour. [Deployment](docs/07-deployment.md) ends with the commands that reproduce this run.
