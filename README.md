# AniMind Backend (Go)

The API for AniMind — a personal anime streaming service with a desktop player (Qt/QML +
libmpv) and a website. This process indexes a media library, authorizes playback, stores
per-user viewing data, runs watch parties, fetches missing episodes over BitTorrent, and
exposes administration.

It is a Go rewrite of the original Node/Express backend with authentication built in rather
than delegated to an external provider. It serves **no frontend**: no browser code, no
templates, no static assets — only JSON, media bytes, and a Socket.IO socket.

## Architecture

```
  desktop player / website
          │
          ▼
       ┌──────┐   :3002 /metrics (loopback only) ──▶ Prometheus ──▶ Grafana
       │ nginx│◀─────────────────────────────────────────────────────┘
       └──┬───┘
          │ :80/:443  (TLS, overwrites X-Forwarded-For)
          ▼
   ┌────────────┐     ┌─────────┐     ┌────────┐
   │ animind API│◀───▶│ Postgres│     │ Redis  │  optional: shared sessions,
   │   :3001    │     └─────────┘     └────────┘  rate limits, metadata cache
   └─────┬──────┘
         │ scan / presign / stream
         ▼
   library on disk or S3 bucket

   watch parties: Socket.IO-compatible socket at /api/socket.io (same port)
```

One binary, one HTTP port, plus an unauthenticated metrics listener on `:3002` that is meant
to stay loopback-only. See [Architecture](docs/01-architecture.md).

## Features

- **Accounts** — email/password (argon2id), Google OAuth, web sessions and desktop device
  pairing with rotating refresh tokens, per-device revocation.
- **Catalog** — shows and episodes indexed from disk or S3, enriched from AniList/MyAnimeList.
- **Playback** — signed stream tickets, byte-range passthrough, presigned S3 redirects,
  on-demand HLS, subtitle extraction, browser-safe audio variants.
- **Watch parties** — Socket.IO rooms with a server-authoritative clock and scheduled group
  starts.
- **Torrent search & downloads** — Torznab search with quotas, a resumable job state machine,
  file selection, and an importer that writes picks into the library (local mode only).
- **User data** — watchlist, resume progress, continue-watching, preferences under `/api/me/*`.
- **Operations** — admin API, Prometheus metrics, storage webhooks, cron scans, Docker stack.

## Requirements

- Go 1.27+ and Postgres 14+ to build and run from source
- ffmpeg/ffprobe on `PATH` when `STORAGE_MODE=local`
- Docker and Compose for the full local stack (optional)

## Quickstart

From source:

```bash
cp .env.example .env        # then edit: DATABASE_URL, DESKTOP_TOKEN_SIGNING_KEY, ...
make run                    # builds, applies migrations, listens on :3001
curl localhost:3001/health
```

With Docker (API + nginx + Postgres + Redis + Prometheus + Grafana):

```bash
make docker-up
curl localhost:8080/health              # API through nginx
# Grafana:    http://localhost:3000  (admin/admin — change it)
# Prometheus: http://localhost:9090
```

Redis is optional: without `REDIS_URL` the API keeps cached state in-process and behaves the
same on a single instance.

The server **fails fast** on a missing `DATABASE_URL`, a short `DESKTOP_TOKEN_SIGNING_KEY`, an
unknown `STORAGE_MODE`, or an unreadable `LOCAL_STORAGE_PATH` — it never starts half-configured.

## Configuration

Everything is environment-driven; [`.env.example`](.env.example) documents every variable.
The essentials:

| Variable | Default | Notes |
| --- | --- | --- |
| `DATABASE_URL` | — | Required. Any Postgres 14+. |
| `DESKTOP_TOKEN_SIGNING_KEY` | — | Required, 32+ chars. Signs desktop tokens **and** playback tickets; rotating it invalidates both. |
| `STORAGE_MODE` | `s3` | `s3` (presigned URLs) or `local` (this process streams bytes and runs ffmpeg). |
| `FRONTEND_URL` | `http://localhost:5173,...` | CORS allowlist. `*` is rejected in production. |
| `REDIS_URL` | empty | Optional shared state. Startup fails if the address does not answer. |
| `WEBHOOK_SECRET` | placeholder | Required; placeholder values are rejected in production. |

Two switches are security-relevant: `TRUST_PROXY_HEADERS` (only behind a proxy that
*overwrites* `X-Forwarded-For`) and `SECURE_COOKIES` (defaults on in production; off exposes
cookies to downgrade attacks).

## API

Machine-readable contracts live in [`docs/openapi.yaml`](docs/openapi.yaml); the narrative
reference is [API reference](docs/03-api-reference.md). Highlights:

```
POST /api/auth/signup | /login | /logout          accounts and sessions
GET  /api/auth/me                                 current account
GET  /api/shows?q=&genre=                         public catalog
GET  /api/shows/{id}                              show + episodes
GET  /api/episodes/{id}/stream-ticket             signed playback ticket
GET  /api/episodes/{id}/stream?st=                media bytes / presigned redirect
GET  /api/hls/{sessionId}/...                     HLS playlist and segments
WS   /api/socket.io                               watch parties
GET/POST/PATCH/DELETE /api/downloads...           torrent downloads
*/api/me/*                                        watchlist, progress, history, preferences
*/api/admin/*                                     admin (requires admin session)
```

## Documentation

| Doc | Topic |
| --- | --- |
| [01-architecture.md](docs/01-architecture.md) | process shape, request lifecycle, package layout |
| [02-authentication.md](docs/02-authentication.md) | accounts, tokens, sessions, OAuth |
| [03-api-reference.md](docs/03-api-reference.md) | endpoints and payloads (+ `openapi.yaml`) |
| [04-storage-and-scanner.md](docs/04-storage-and-scanner.md) | S3/local storage, library indexing |
| [05-streaming.md](docs/05-streaming.md) | tickets, ranges, HLS, subtitles, audio variants |
| [06-watch-parties.md](docs/06-watch-parties.md) | SyncPlay protocol and rooms |
| [07-deployment.md](docs/07-deployment.md) | VPS/systemd/nginx, backups, verification harnesses |
| [08-client-integration.md](docs/08-client-integration.md) | desktop pairing, migration from the Node backend |
| [09-security.md](docs/09-security.md) | threat model and controls |
| [10-caching-and-observability.md](docs/10-caching-and-observability.md) | Redis, metrics, Grafana |
| [11-downloads.md](docs/11-downloads.md) | torrent search and downloads |

## Development

```bash
make build        # compile to build/animind-server
make test         # unit tests (integration tests skip without a database)
make test-db      # full suite against POSTGRES_TEST_DATABASE_URL
make test-cache   # ... plus REDIS_TEST_URL
make vet cover    # static check and coverage
make docker-up    # full local stack
```

Live verification harnesses run against a running server:

```bash
python3 scripts/e2e_verify.py --base http://localhost:3001    # auth, catalog, playback, admin
python3 scripts/verify_media.py --base http://localhost:3001  # subtitles, variants, HLS (needs ffmpeg)
```

## Security

Report vulnerabilities through GitHub's private advisory form — see
[SECURITY.md](SECURITY.md). Do not open a public issue. The defensive audit of this repository,
its findings and their remediation status are under [`security-audit/`](security-audit/).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Open an issue before changing a route, a payload
field, or the watch-party protocol — shipped clients read those shapes.

## Known limitations

- Torrent search and downloads are **local-mode** features; in `s3` mode those routes answer
  `501`.
- `GET /api/shows` reports an unfiltered `total` alongside the filtered page.
- `make lint` has known findings that CI does not gate on yet (see `.golangci.yml`).

## Licence

[MIT License](LICENSE). [NOTICE](NOTICE) records third-party components and their licences.
