# 10. Caching and observability

Redis is optional. Metrics are not collected unless a scraper is configured. This document is
the whole shared-state surface: what is cached, for how long, what invalidates it, and what the
dashboard reads.

| Container | Purpose | Reachable from |
| --- | --- | --- |
| `nginx` | TLS, websocket upgrade, the only port a client touches | anywhere |
| `api` | this backend | nginx, and Prometheus on :3002 |
| `postgres` | the source of truth | the API |
| `redis` | cached credentials, rate-limit windows, AniList documents | the API |
| `prometheus` | scrapes `animind-api:3002/metrics` every 10 s | Grafana on loopback :9090 |
| `grafana` | the AniMind Operations dashboard, provisioned | loopback :3000 |

---

## What is cached, and why that one thing

`internal/cache` exposes two small interfaces. Everything about Redis — namespacing, the Lua
that makes a window atomic, the set that makes group eviction possible — stays inside it.

```go
type KV interface {
    Get(ctx, key) ([]byte, bool, error)
    Set(ctx, key, value, ttl, groups...) error
    Delete(ctx, keys...) error
    DeleteGroup(ctx, groups...) error
}

type Limiter interface {
    Allow(ctx, key, limit, window) (Decision, error)
}
```

Three seams use them. Each was chosen for a measured cost, not as a general-purpose cache.

### 1. Resolved credentials

Every authenticated request and every watch-party socket connect resolved its token with two
indexed SELECTs — the session row, then the user row — and `/api/me` added a third. Authorization
was the most frequent query in the deployment.

- **Key**: `credential:<web|desktop>:<sha256 of the token>` — the same hash the database stores,
  so a cache key leaks nothing about the token it stands for.
- **TTL**: `min(CACHE_SESSION_TTL_SECONDS, touchInterval, the row's own expiry)`. The second term
  is what keeps sliding expiry honest; the third is what stops the cache extending a session.
- **Evicted by**: logout, revoke-by-id, logout-everywhere, password change, password reset,
  account deletion, desktop rotation, desktop revoke, profile update, and the admin-flag write
  from `PATCH /api/admin/users/{id}`.

Revocation paths know the *account*, never the token hash, so entries are written into the
account's group and the group is deleted. That indirection is the whole design: it is what lets
`DELETE /api/auth/sessions/{id}` evict an entry whose key no caller still holds.

The residual window is a resolution already in flight when the revocation lands — its entry can
be written after the eviction. The ceiling on that is one TTL, which is why the TTL is a minute
and not an hour. See `internal/auth/credential_cache_test.go` for the paths that prove eviction
happens, and the row-expiry test for the bound.

### 2. Rate-limit windows

`internal/httpx/ratelimit.go` used to keep a `map[string]*window` per process. Behind nginx with
two replicas that is two independent budgets, and the client cannot say which instance answered:
the configured limit silently becomes limit × replicas. Counting now goes through the store as
one `INCR` plus a `PEXPIRE` on first hit, in a Lua script so a crash between the two cannot leave
a counter that never resets.

When Redis does not answer inside `CACHE_COMMAND_TIMEOUT_MS`, the verdict comes from an in-process
window and `animind_rate_limit_fallbacks_total` climbs. Fail-open over fail-closed: an unreachable
cache should cost latency, not lock every client out of a healthy API.

### 3. AniList documents and scan titles

`POST /api/anilist` was a blind relay: one outbound call per site click, no cache, no backoff, and
AniList's rate limit therefore was the clients' capacity. Read-only documents are now cached for
`CACHE_METADATA_TTL_HOURS` under the SHA-256 of the canonical request body, and `X-Cache: HIT|MISS`
says which happened. Only a `200` carrying `data` and no `errors` is stored — AniList answers some
rejections with `200` and an error array, and caching that would hide a title for half a day.

The scanner's title lookup gained the same store behind its in-process map, so a restart in the
middle of indexing a hundred-episode show no longer re-queries every title it had already
resolved. Titles use `METADATA_CACHE_TTL_HOURS`, and a title no provider matched is kept in the
process only, so a shared "nothing found" cannot outlive the run that learned it.

### Deliberately not cached

- **Catalog listings.** `/api/shows` is a count plus a select; `/api/shows/{id}` is the show and
  its episodes. There is no N+1 to hide. The slow part is `ILIKE '%term%'`, which cannot use a
  plain index — a trigram or FTS index fixes that permanently, while a cache would only make the
  wrong query fast for a while and stale for the rest.
- **Playback tickets.** They are HMAC-verified with no database read at all, so there is nothing
  to remove. Caching them would only be worth it to make a ticket single-use, which is a different
  feature with a different cost.
- **Watch-party state.** Rooms live in memory on purpose. The gate and the scheduled start are
  synchronised to sub-100 ms, and a network round trip per decision would attack the number the
  last upgrade was measured against. Multi-replica parties need pub/sub, not a cache.

---

## Failure behaviour

| Failure | What the API does |
| --- | --- |
| `REDIS_URL` set, unreachable at boot | refuses to start, naming the address |
| Redis unreachable at runtime | credential and metadata reads miss and go to the source; rate limits count locally and export the fallback count |
| An entry that will not decode (older build) | deleted, treated as a miss |
| Redis flushed or restarted | refilled on the next requests; one rate-limit window is lost |
| AniList upstream down | 502 as before, and nothing is stored |

Nothing cached here is a source of truth. Every key has an authoritative path behind it.

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `REDIS_URL` | empty | empty means in-process, and the same code paths run |
| `CACHE_NAMESPACE` | `animind` | letters, digits, `-`, `_`; anything else is stripped so a prefix cannot forge a separator |
| `CACHE_SESSION_TTL_SECONDS` | `60` | clamped down to the touch interval by the auth layer |
| `CACHE_METADATA_TTL_HOURS` | `12` | proxied AniList documents (`METADATA_CACHE_TTL_HOURS` still governs scan-title results) |
| `CACHE_DIAL_TIMEOUT_MS` | `500` | boot only |
| `CACHE_COMMAND_TIMEOUT_MS` | `250` | every read, write and eviction |
| `CACHE_MAX_CONNECTIONS` | `20` | pool ceiling, so a slow Redis cannot consume the process |
| `METRICS_ENABLED` | `true` | |
| `METRICS_ADDR` | `127.0.0.1:3002` | loopback by default: the listener is unauthenticated |
| `METRICS_PATH` | `/metrics` | |

---

## Metrics

The scrape target is a separate listener, not an API route. It is unauthenticated because the
route table has no session on a Prometheus poll, and it is safe only because nginx does not proxy
it and compose does not publish it.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `animind_http_requests_total` | counter | `route`, `method`, `status` | matched route *pattern*, exact status code |
| `animind_http_request_duration_seconds` | histogram | `route`, `method` | handler entry to response finish |
| `animind_http_requests_in_flight` | gauge | `method` | handlers running now |
| `animind_cache_operations_total` | counter | `result`, `backend` | hit, miss, set, evict, error, allow, deny, fallback |
| `animind_cache_shared` | gauge | | 1 when state crosses processes, 0 when it lives in this one |
| `animind_rate_limit_fallbacks_total` | counter | | verdicts taken locally because Redis did not answer |
| `animind_db_connections_{total,active,idle,max}` | gauge | | pgx pool, so a queueing request is distinguishable from a slow query |
| `animind_library_entries` | gauge | `kind` | shows, episodes, users; refreshed at most once a minute |
| `animind_watchparty_{rooms,peers}` | gauge | | live rooms and sockets |
| `animind_downloads_active` | gauge | | jobs in a non-terminal state plus ready/paused, from the quota's own count |
| `animind_downloads_bytes_total` | counter | | bytes completed across all jobs |
| `animind_downloads_failed_total` | counter | | jobs that left the state machine as failed |
| `torrent_search_seconds` | histogram | | one /api/torrents/search, indexer fan-out included |
| `go_*`, `process_*` | | | runtime and CPU |

`route` is the chi pattern (`/api/hls/{sessionId}/{segment}`), read after the handler runs. Label
values are never a client-supplied path, so a probe sweep cannot mint unbounded series — which is
the one way a metrics endpoint becomes the outage it is monitoring.

### Dashboard

`deploy/grafana/dashboards/animind.json` is provisioned as **AniMind → AniMind Operations** with
the Prometheus data source, so `docker compose up` ends with something on screen rather than an
empty Grafana. Panels: uptime, 5xx rate, credential cache hit ratio, whether state is shared,
watch parties, library size, requests per second by route, p50/p95 by route, status codes,
denials against 429s, cache operations, local-window fallbacks, pool, process. A downloads row
(active jobs, throughput, failures, search latency) lands together with the download metrics'
wiring.

Two readings worth knowing:

- **Hit ratio near zero on a busy API** — sessions are not being cached. Check
  `animind_cache_shared` and the error result in `animind_cache_operations_total`.
- **`result="deny"` climbing while `status="429"` is flat** — the limiter counts but the response
  never happens, which means the fallback path answered. Look at
  `animind_rate_limit_fallbacks_total`.

Related: [Architecture](01-architecture.md), [Authentication](02-authentication.md),
[Deployment](07-deployment.md), [Security](09-security.md).
