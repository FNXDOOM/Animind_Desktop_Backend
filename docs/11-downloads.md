# Torrent downloads

The server can search a Torznab indexer and download anime releases into the library with the
built-in torrent client. Everything here is a local-storage feature: in `s3` mode every download
route answers 501 `downloads_unavailable` and the torrent engine is never constructed.

The operator is responsible for downloading only content they are licensed to have. Do not hide
download traffic, and do not use the client to bypass network blocks set up by the network's
owner.

## Setup

Point `TORZNAB_URL` at a Torznab endpoint — Prowlarr's or Jackett's per-indexer `api` URL works
directly (`http://host:9696/api/v2/indexers/prowlarr/results/torznab/api` style) — and set
`TORZNAB_API_KEY`. An empty `TORZNAB_URL` disables search: `/api/torrents/search` answers
501 `indexer_not_configured`. The URL must be http(s) and must not carry userinfo; the API key
travels as a query parameter, matching how Torznab clients authenticate.

Search results are ranked (seeders, trusted uploaders, parse quality), deduped by info hash, and
cached for `TORRENT_SEARCH_CACHE_MINUTES` (default 10). Every row carries an opaque id; the
magnet stays inside the server, and a download is created by referencing the opaque id only. Ids
expire with the cache — a `POST /api/downloads` that references an expired id answers
410 `result_expired`.

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `TORZNAB_URL` | empty | http(s) Torznab endpoint; empty disables search |
| `TORZNAB_API_KEY` | empty | required when `TORZNAB_URL` is set |
| `TORRENT_SEARCH_CACHE_MINUTES` | 10 | result and search-page cache TTL |
| `DOWNLOADS_DIR` | `<LOCAL_STORAGE_PATH>/.incoming` | must stay under the library root |
| `DOWNLOADS_MAX_ACTIVE` | 2 | global concurrent-jobs ceiling |
| `DOWNLOADS_MAX_ACTIVE_PER_USER` | 3 | per-account ceiling |
| `DOWNLOADS_MAX_BYTES_PER_JOB` | 25 GiB | one torrent's total size cap |
| `DOWNLOADS_MIN_FREE_GB` | 20 | headroom the download root must keep |
| `DOWNLOADS_DOWN_KBPS` / `UP_KBPS` | 0 / 500 | throttles; 0 = unlimited |
| `DOWNLOADS_SEED_RATIO` / `SEED_MINUTES` | 0 / 0 | seeding stops at completion when both are 0 |
| `DOWNLOADS_DHT` / `PEX` / `UTP` | true | transport toggles |
| `DOWNLOADS_MAX_FILES` | 500 | entries extracted from one torrent |

## Quotas and states

A job walks the states `queued → fetching_metadata → (awaiting_selection →) downloading →
verifying → importing → ready`, with `paused`, `failed` and `cancelled` off the main path.
Multi-file torrents pause in `awaiting_selection` until the client picks files with
`PATCH /api/downloads/{id}`; single-episode torrents select themselves and roll on.

**The quota counts every non-terminal state plus `ready` and `paused` as active.** A user at
their `DOWNLOADS_MAX_ACTIVE_PER_USER` cap who has finished (`ready`) or paused jobs is blocked
from starting new ones until they delete a job with `DELETE /api/downloads/{id}` — cancelling a
finished job just removes the row, so it is the intended way to free a slot. The same set is the
dedupe key: a torrent in any active state cannot be queued twice by the same account, and the
409 response names the existing job id.

Disk is checked before anything is written: the root must keep `DOWNLOADS_MIN_FREE_GB` beyond the
torrent's size (507 `insufficient_disk`), and jobs above `DOWNLOADS_MAX_BYTES_PER_JOB` are
refused up front (400 `job_too_large`).

## What happens on disk

- Downloads land in `DOWNLOADS_DIR/<jobID>/` — the `.incoming` directory the scanner skips, so a
  half-finished torrent never appears in the catalog.
- When a job finishes, the importer **moves** the selected video and subtitle files into the
  library as `Shows/<title>/Season NN/...`, links them to a show row when the title resolves, and
  stamps `completed_at`. One library scan runs after imports and after a delete-with-files, so
  episode rows appear (or disappear) without operator action.
- `DELETE /api/downloads/{id}?deleteFiles=true` drops the engine's data and the job directory; a
  plain delete only removes the row, leaving any downloaded files in place.
- Imports never overwrite: a conflicting destination file fails the job with
  `import_conflict`.

Failed jobs carry a machine-readable `error`: `metadata_timeout` (the swarm never delivered the
metainfo), `engine_disabled`, `no_files`, `import_conflict`, or `download_failed`.

## Events

`GET /api/downloads/events` is an SSE stream: an `event: snapshot` per known job on connect,
then `event: job` frames on every state change and progress tick (coalesced to at most one per
second per job), with a comment heartbeat every 15 seconds. Two concurrent streams per user,
100 in total; a refused connection answers 503. Frames carry state, progress and transfer rates
— never magnets or filesystem paths.

## Seeder helper

Task 11 ships `cmd/animind-seed`, a small client that builds a torrent from a directory and seeds
it on loopback so the verification script can exercise a full job without a public swarm. Point
a stub Torznab at its magnet and the whole pipeline runs offline.

Related: [API reference](03-api-reference.md), [Security](09-security.md),
[Caching and observability](10-caching-and-observability.md), [Deployment](07-deployment.md).
