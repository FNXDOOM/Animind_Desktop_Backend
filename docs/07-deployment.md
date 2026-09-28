# 7. Deployment

Target deployment is a single VPS: one process, one Postgres, one nginx. This is not a
horizontally-scaled service — rate limits and watch rooms are per-process (see
[Architecture](01-architecture.md#deliberate-limitations)).

## Build

```bash
make build                       # -> build/animind-server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o animind-server ./cmd/server
```

The binary is static and needs no CGO. `ffprobe` and `ffmpeg` must be on `PATH` (or set
`FFPROBE_PATH` / `FFMPEG_PATH`) when `STORAGE_MODE=local`; without them, subtitle extraction and
audio variants degrade to warnings and HLS sessions fail with a clear error. Nothing else in the
API depends on them.

## systemd

```ini
# /etc/systemd/system/animind-backend.service
[Unit]
Description=AniMind backend (Go)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=animind
Group=animind
WorkingDirectory=/opt/animind-backend
EnvironmentFile=/etc/animind/backend.env
ExecStart=/opt/animind-backend/animind-server
Restart=on-failure
RestartSec=3
TimeoutStopSec=40          # leaves room for graceful shutdown to kill ffmpeg sessions
KillMode=mixed

NoNewPrivileges=true
PrivateTmp=false           # HLS segments go under the shared temp dir the player reads
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/mnt/anime /tmp
ProtectKernelTunables=true
RestrictAddressFamilies=AF_INET AF_INET6
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now animind-backend
journalctl -u animind-backend -f
```

`ProtectSystem=strict` with `ReadWritePaths` for the library and temp directory is what makes a
compromise of the process unable to write anywhere else. `LimitNOFILE` matters: each range
request and each HLS segment fetch is a descriptor, and a library being scanned walks many files.

The service stops by signal. `main.shutdown()` stops the scheduler, closes every watch room, stops
accepting HTTP (in-flight streams get 30 s), then kills every ffmpeg process and removes its
segments. Do not send `SIGKILL` in a hurry: that leaves orphaned ffmpeg processes holding temp
files.

## nginx

The socket and the API share one port and one prefix, so one location block covers both. Range
requests and long media responses need buffering off.

```nginx
upstream animind { server 127.0.0.1:3001; }

server {
  listen 443 ssl http2;
  server_name api.example.com;

  ssl_certificate     /etc/letsencrypt/live/api.example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/api.example.com/privkey.pem;

  add_header Strict-Transport-Security "max-age=31536000" always;
  # The API returns JSON only; there is nothing here worth framing.
  add_header X-Frame-Options "DENY" always;

  # Keep the client's real address for the rate limiter, then set TRUST_PROXY_HEADERS=true.
  proxy_set_header X-Forwarded-For $remote_addr;
  proxy_set_header X-Real-IP       $remote_addr;
  proxy_set_header Host            $host;

  location / {
    proxy_pass http://animind;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection $http_connection;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_buffering off;
  }

  # Do not buffer media: a 4 GB file through nginx's temp files is a disk problem, not a
  # memory one.
  location ~ ^/api/episodes/.+/stream {
    proxy_pass http://animind;
    proxy_http_version 1.1;
    proxy_buffering off;
    client_max_body_size 1m;
  }

  location /api/socket.io {
    proxy_pass http://animind;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 75s;         # longer than the 25 s ping interval
    proxy_buffering off;
  }
}
```

Set `TRUST_PROXY_HEADERS=true` **only** when the proxy overwrites `X-Forwarded-For` the way above.
Otherwise a client can send its own value and buy itself an unlimited rate-limit bucket. If the
process is reachable directly, it is not behind that proxy — bind it to `127.0.0.1` or a private
interface.

`PUBLIC_URL` must match the hostname clients use, since it is the token issuer and the OAuth
callback base.

## Docker

```bash
docker compose up --build              # Postgres + API on :3001
docker compose up -d db
DATABASE_URL=postgres://animind:animind@localhost:5432/animind animind-server --migrate-only
```

The image is Alpine plus ffmpeg, runs as a non-root user, and healthchecks `/health` — which
answers 503 when the database is unreachable, so the check means something. For an S3 deployment
no library volume is needed.

## Database

Postgres 14+. Migrations apply on boot (`DB_AUTO_MIGRATE=true`) or explicitly:

```bash
animind-server --migrate-only
psql "$DATABASE_URL" -f migrations/0001_core.sql   # the files are plain SQL too
```

`schema_migrations` records what has run; each file applies inside its own transaction, so a
partial file rolls back and retries on the next boot rather than leaving the schema in an unknown
state.

Pooling: `DB_MAX_CONNS` (12) is enough for a single instance. Streaming responses do not hold a
database connection — the ticket or session was already verified before the file is opened.

### Backups

```bash
pg_dump "$DATABASE_URL" | gzip > animind-$(date +%F).sql.gz
```

Everything user-owned lives in Postgres: accounts, sessions, watchlist, resume positions,
continue-watching history, preferences, and the indexed library. Media files are the other half —
in local mode they are ordinary files, so back up `LOCAL_STORAGE_PATH` with whatever the host uses
(rclone, restic, ZFS snapshot). Exclude `.animind-audio-cache`: it is derived and rebuilds itself.

To move a library between hosts, copy the files and re-scan: the catalog regenerates from the
filenames plus AniList. Watchlist, progress and history are keyed on AniList ids, so they survive
a re-scan.

## Secrets

| Secret | Scope |
| --- | --- |
| `DESKTOP_TOKEN_SIGNING_KEY` | Desktop access tokens **and** playback tickets. Rotating invalidates both. |
| `WEBHOOK_SECRET` | Storage webhook only. |
| `DATABASE_URL` | Includes the password; keep it out of the repository. |
| `S3_SECRET_ACCESS_KEY` | Bucket read; a scoped read-only key is enough. |
| `GOOGLE_CLIENT_SECRET` | OAuth only. |

Store them in `/etc/animind/backend.env` (mode `0600`, owned by root) rather than a user dotfile,
and never commit them — `.gitignore` excludes `.env`, and `.dockerignore` keeps it out of the image
context.

## Verifying a deployment

The repository ships two live harnesses. Run them against a staging instance, not production:
`e2e_verify.py` creates and deletes accounts and triggers scans.

```bash
python3 scripts/e2e_verify.py --base https://api.staging.example
python3 scripts/verify_media.py --base http://localhost:3011 --container animind-api
```

`verify_media.py` checks the parts that need the real binaries: audio-track probing, embedded
subtitle extraction to WebVTT, the cached audio-variant transcode, and an on-demand HLS session
create → playlist → segment → seek → heartbeat → destroy. It therefore needs local storage mode
with a real video file; a Matroska with an ASS subtitle track and two audio streams exercises all
four paths. Generate one in a throwaway container:

```bash
docker run --rm -v "$LIB:/mnt/anime" alpine:3.20 sh -c \
  'apk add --no-cache ffmpeg && ffmpeg -v error -f lavfi -i testsrc=duration=8:size=320x240:rate=25 \
   -f lavfi -i sine=frequency=440:duration=8 -f lavfi -i sine=frequency=880:duration=8 \
   -map 0:v -map 1:a -map 2:a -c:v libx264 -pix_fmt yuv420p -c:a:0 aac -c:a:1 libopus \
   -metadata:s:a:1 language=jpn -y "/mnt/anime/Demo/Demo - 01 (1080p).mkv"'
```

Quick manual smoke test:

```bash
curl -fsS https://api.example.com/health                 # status ok, database ok
curl -s -XPOST https://api.example.com/api/webhooks/storage -H 'x-webhook-secret: wrong' -i | head -1
```

Expected: healthy JSON, and `401`. A `200` on the webhook means `WEBHOOK_SECRET` is not what
you think it is. Note that a host without a database and without ffmpeg can prove the build but
not the behaviour — five defects in this port surfaced only when the real Postgres and the real
ffmpeg were in the loop (see the table in the README).

## Operational log lines

| Line | Meaning |
| --- | --- |
| `server listening addr=:3001 storage_mode=local` | Boot complete |
| `migration applied migration=0001_core.sql` | Schema advanced |
| `library scan finished scanned=… indexed=… removed=… errors=…` | Scan outcome |
| `watch party created/closed room=…` | Room lifecycle |
| `evicting HLS session to make room` | `HLS_MAX_CONCURRENT_SESSIONS` reached |
| `password reset token issued token=…` | Recovery was requested; deliver the token |
| `account created` / `account deleted` / `desktop device paired` | Security-relevant events |
| `panic recovered … stack=` | A defect: the request got 500 and the stack is logged |

Related: [Architecture](01-architecture.md), [Security](09-security.md).
