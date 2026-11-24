# Infrastructure & Deployment Audit

**Files reviewed:** `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `deploy/nginx/nginx.conf`,
`deploy/prometheus/prometheus.yml`, `deploy/grafana/provisioning/*`, `.github/workflows/ci.yml`,
`.env.example`, `SECURITY.md`.
**Not in scope:** the host OS, the TLS terminator (not present), the cloud provider's IAM policy,
the actual bucket policy, and the network firewall.

---

## 1. Container image

| Control | Status | Evidence / comment |
|---|---|---|
| Multi-stage build | **Good** | `golang:1.27-alpine` build stage, `alpine:3.20` runtime |
| Static binary | **Good** | `CGO_ENABLED=0`, `-trimpath`, `-s -w` |
| Non-root user | **Good** | `adduser -S animind` + `USER animind` |
| Minimal runtime | **Good** | only `ffmpeg`, `ca-certificates`, `tzdata` added |
| Secrets excluded from build context | **Good** | `.dockerignore` excludes `.env`, keys, `.git`, `build/`, `library/` |
| Base image freshness | **Weak** | `alpine:3.20` is at/near end of its support window (released May 2024); `ffmpeg` in an EOL base stops receiving fixes |
| Digest pinning | **Weak** | both stages use tags, not `@sha256:` |
| Read-only root filesystem | Not set | runtime writes `/tmp` (HLS), the library and possibly `.torrent.db`; a read-only root plus explicit writable mounts would reduce blast radius |
| Capability drop / no-new-privileges | Not set | no `--cap-drop=ALL`, `security_opt: no-new-privileges` |
| Resource limits | Not set | no CPU/memory/PID limits on `api`; ffmpeg and torrent clients can starve the host |
| Healthcheck | **Good** | `wget /health`, 30 s interval |
| Image scanning / SBOM | Missing | see `DEPENDENCY_AUDIT.md` |

**Risk:** the API image is well-built for a personal deployment. The main exposure is a stale
runtime base with ffmpeg, which processes untrusted media.

**Recommended run configuration:**

```yaml
services:
  api:
    read_only: true
    tmpfs: ["/tmp:size=512m,mode=1777"]
    security_opt: ["no-new-privileges:true"]
    cap_drop: ["ALL"]
    deploy:
      resources:
        limits: { cpus: "2.0", memory: 2g, pids: 512 }
    volumes:
      - ${ANIMIND_LIBRARY:-./library}:/mnt/anime
```

(Adjust the memory/tmpfs values to the library and transcode profile. The API needs a writable
`/tmp` for HLS and a writable library for subtitles/audio variants; S3 mode needs neither.)

## 2. Docker Compose stack

The file is explicitly documented as a local-development stack. It is nevertheless the only
deployment recipe in the repository, and operators copy it. Findings are therefore framed as
"do not deploy this as-is".

| Service | Binding | Credential / setting | Assessment |
|---|---|---|---|
| `nginx` | `0.0.0.0:${NGINX_PORT:-8080}` | none | Publishes on all interfaces; HTTP only (no TLS listener). **F-05** |
| `api` | `127.0.0.1:3001` | `ENV=development`; placeholder signing key; placeholder webhook secret | Direct API port is loopback-only (good); hardening disabled by `ENV` |
| `db` | `127.0.0.1:5432` | `POSTGRES_USER=animind`, `POSTGRES_PASSWORD=animind`, DB `animind`, `sslmode=disable` | Loopback-only; default credentials; no TLS. Acceptable only for local dev |
| `redis` | `127.0.0.1:6379` | no auth; persistence off; 192 MB maxmemory | Loopback-only; `maxmemory`+volatile-lru is good for F-03 |
| `prometheus` | `127.0.0.1:9090` | `--web.enable-lifecycle` | Loopback-only; lifecycle endpoint is unauthenticated |
| `grafana` | `127.0.0.1:3000` | `admin` / `admin` default (env-overridable) | Loopback-only; default password must be changed |

**Positive controls:** all non-nginx services bind to loopback; the metrics port is not published;
the library is a bind mount; volumes are named; healthchecks and `depends_on` conditions exist.

**Gaps:**

1. No TLS anywhere. The API sends session cookies over plaintext unless a TLS terminator is added
   externally; with `ENV=development` the cookie is not `Secure` either.
2. Known placeholder secrets (`DESKTOP_TOKEN_SIGNING_KEY`, `WEBHOOK_SECRET`) and default DB
   credentials; the app only rejects the literal `dev-secret` in production.
3. No resource limits (`cpus`, `mem_limit`, `pids_limit`) — a single torrent or HLS flood can
   starve the host; F-02/F-03 are amplified by this.
4. No log rotation configuration for the nginx access/error logs or the API's stdout; a long-lived
   container can fill the disk. (Docker's default `json-file` driver has no size cap unless
   configured.)
5. No restart backoff beyond `unless-stopped`; a crash loop from OOM will restart indefinitely.
6. `sslmode=disable` for Postgres is correct for a loopback compose network but must not be
   copied to a remote database.
7. Grafana provisioning binds to Prometheus with `access: proxy`, good; the admin password is the
   only gate.
8. `prometheus --web.enable-lifecycle` allows unauthenticated `/-/reload` and shutdown endpoints;
   loopback-only, but do not publish it.

## 3. nginx

| Control | Status | Comment |
|---|---|---|
| `server_tokens off` | **Good** | version banner suppressed |
| Client body limit | **Good** | 8 MiB at nginx, 1 MiB enforced by the API |
| Timeouts | **Good** | body/keepalive/read timeouts set; websocket read timeout 75 s > 3 ping intervals |
| X-Forwarded-For handling | **Good** | **overwrites** with `$remote_addr` in every location; this is the property that makes `TRUST_PROXY_HEADERS=true` safe |
| WebSocket upgrade | **Good** | `map $http_upgrade`, `proxy_http_version 1.1`, `Connection $connection_upgrade` |
| Media buffering | **Good** | `proxy_buffering off`, `proxy_request_buffering off`, 300 s read timeout |
| Metrics blocked | **Good** | `location = /metrics { return 404; }` |
| TLS | **Absent** | `listen 80;` only; no `ssl_certificate`, no HTTP→HTTPS redirect |
| HSTS / security headers | **Absent** | no `Strict-Transport-Security`, `X-Content-Type-Options`, `Referrer-Policy`, CSP at the proxy (the API sets some) |
| Rate limiting at the edge | **Absent** | all limiting is in the Go process; a flood still reaches the API and sockets |
| Connection limits | **Absent** | no `limit_conn`/`limit_req`; the socket path is especially exposed (F-02) |
| Access log redaction | **Partial** | default combined log records the full request URI, including `?st=` playback tickets and OAuth `?code=`/`?state=` on the callback. `SECURITY.md` warns about `?st=` in logs but the shipped nginx config does not redact it |
| Request smuggling | No conflicting `Content-Length`/`Transfer-Encoding` handling in this config | Go's server rejects ambiguous framing; keep nginx updated |

**Recommendation:** add a TLS server block (or terminate TLS in a dedicated proxy), an HTTP→HTTPS
redirect, HSTS, and `limit_conn`/`limit_req` for `/api/socket.io` and `/api/auth/*`. Configure an
access-log format that logs `$uri` without `$args` for `/api/`, or mask `st`, `code`, and `state`.

## 4. Metrics and observability

- `METRICS_ENABLED=true`, `METRICS_ADDR=127.0.0.1:3002` by default, separate listener.
- The listener is unauthenticated by design and exports route/method/status counts, durations,
  in-flight requests, DB pool stats, library counts, watch-party counts, cache stats and download
  counters. No user identifiers or tokens are exported.
- Compose binds it to `0.0.0.0:3002` **inside the compose network** and does not publish it;
  Prometheus scrapes `api:3002`. This is safe only while the network is private.
- nginx returns 404 for `/metrics`; the API does not proxy it.
- Grafana admin defaults to `admin`/`admin`; change both.

**Residual risk:** if an operator sets `METRICS_ADDR=0.0.0.0:3002` and exposes the port, the
library size, traffic patterns and party counts become public. That is documented; no code change
is required beyond keeping the default.

## 5. Database and cache

| Control | Status | Comment |
|---|---|---|
| DB reachable only from compose/loopback | **Good** | compose publishes `127.0.0.1:5432` |
| DB TLS | Dev only | `sslmode=disable` in compose; use `require`/`verify-full` for a remote DB |
| DB role privileges | Not in repo | the schema is owned by one role; least privilege is an operator task |
| Migrations | **Good** | embedded, ordered, applied on boot by default; `--migrate-only` exists |
| Schema constraints | **Good** | UUID PKs, FKs with cascade, unique constraints on email/session hashes/active downloads, state CHECK |
| Backup / restore | Not in repo | operator responsibility; document and test |
| Redis auth | None in compose | loopback-only; if moved off-host, require TLS + ACL |
| Redis durability | Disabled intentionally | cache/limits only; losing it is safe (documented) |
| Rate-limit store failure | Fails open | documented trade-off; see `SECURITY_FINDINGS.md` verified controls |

## 6. S3 / object storage

- `STORAGE_MODE=s3` requires `S3_BUCKET_NAME`; credentials are static env values (no IMDS/role
  assumption).
- Presigned GET URLs default to **4 hours** (`S3_PRESIGN_EXPIRES=14400`); a leaked URL grants the
  object for its lifetime.
- The bucket name in `episodes.bucket_name` can override the configured bucket; only server-side
  scanner code writes that column.
- No client-side bucket enumeration or public-listing call exists.
- **Not assessable:** the actual bucket policy, Block Public Access, key rotation, and whether the
  credentials are scoped to the single bucket. These are deployment controls and should be tested
  outside the repository review.
- **P-05:** if the bucket policy allows anonymous `s3:GetObject`, every media file is public; this
  audit cannot verify it.

## 7. Host and runtime

- The API listens on `:3001` (all interfaces) inside the container; compose maps it to loopback.
- The process needs:
  - read access to the library (and write access for subtitles/audio variants in local mode);
  - a writable temp dir for HLS;
  - a writable downloads dir under the library;
  - outbound HTTPS to AniList/MAL/Google/OpenRouter and outbound BitTorrent (TCP/UDP) when
    downloads are enabled.
- `DOWNLOADS_DHT/PEX/UTP` default true; UPnP/NAT port mapping is explicitly disabled in the
  engine (`NoDefaultPortForwarding`). Good.
- No seccomp/AppArmor profile beyond Docker defaults; ffmpeg is the highest-risk child process.
- Logs go to stdout (structured) and nginx files; no retention/rotation policy is configured.

## 8. Infrastructure findings map

| Finding | Infrastructure relevance |
|---|---|
| F-02 socket exhaustion | no nginx `limit_conn`/`limit_req`; compose has no resource limits |
| F-03 AniList cache | Redis `maxmemory` exists in compose; a bare Redis or local cache is unbounded by bytes |
| F-05 insecure defaults | compose `ENV=development`, sample secrets, HTTP-only nginx, default DB/Grafana passwords |
| F-13 dependencies | EOL Alpine runtime, tag-pinned images |
| F-18 CI/images | actions by tag, no scanning/SBOM, images not digest-pinned |
| F-19 error details | non-production deployments leak internal error text |
| F-20 PII in logs | nginx logs full query strings; API logs the recovery email |

## 9. Hardening checklist delta

`SECURITY.md` already lists ten hardening steps. The shipped files satisfy some of them by
default and rely on the operator for the rest:

| SECURITY.md step | Satisfied by default? |
|---|---|
| 1. TLS in front of nginx, `SECURE_COOKIES` on | **No** — nginx is HTTP-only; cookie Secure depends on `ENV` |
| 2. Metrics on loopback | **Yes** — API default; compose network-only |
| 3. Real `WEBHOOK_SECRET` | **No** — compose ships a placeholder |
| 4. Random `DESKTOP_TOKEN_SIGNING_KEY` | **No** — compose ships a placeholder |
| 5. `ALLOW_SIGNUP=false` once accounts exist | Operator |
| 6. Exact `FRONTEND_URL` | Operator; `*` only rejected in production |
| 7. Postgres/Redis on loopback | **Yes** in compose |
| 8. Non-root API user | **Yes** |
| 9. Prefer S3 if the library must not be writable | Operator |
| 10. Recovery delivery integration | **No** — not implemented (F-14) |
