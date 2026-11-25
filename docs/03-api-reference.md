# 3. API reference

Base URL is the API origin (`.env` `PUBLIC_URL`, e.g. `https://api.example.com`). All paths
begin with `/api` except `/health`. JSON only; request bodies are capped at 1 MiB and unknown
fields are rejected so a client typo fails loudly instead of being ignored.

**Authentication**

| Scheme | How |
| --- | --- |
| Session | `Authorization: Bearer as_...` **or** cookie `animind_session` |
| Desktop | `Authorization: Bearer adk_...` |
| Media bytes | `?st=<playback ticket>` (no header possible from a media element) |

**Error shape** — every non-2xx body is `{"error": "..."}`, optionally with `details`
(non-production only) and a stable `code`:

```json
{ "error": "Invalid or expired token.", "code": "rate_limited" }
```

`code` values in use: `rate_limited`, `scan_in_progress`, `hls_capacity`, `hls_timeout`,
`anilist_unreachable`, `oauth_not_configured`, `room_ended`.

**Rate limits** are per client IP and per route, per minute: public reads 120, signup 10,
login 20, account 5, admin 60, user data 240, stream ticket 120, media stream 600, HLS session
60, HLS segment 1200, webhook 30. A refusal sends `Retry-After`.

---

## Health

### `GET /health` (also `/api/health`)

No auth. Answers `degraded` with **503** when the database is unreachable, so a healthcheck
can act on it.

```json
{ "status": "ok", "timestamp": "2026-09-27T12:00:00Z", "uptime": "3h11m2s",
  "storage": "local", "database": "ok", "watch_parties": true }
```

---

## Catalog (public)

### `GET /api/shows`

| Query | Default | Notes |
| --- | --- | --- |
| `limit` | 50 | clamped to 1-200 |
| `offset` | 0 | |
| `q` | | matches title, studio or synopsis (ILIKE) |
| `genre` | | exact member of the `genres` array |

```json
{ "data": [ { "id": "uuid", "title": "Frieren: Beyond Journey's End",
              "synopsis": "...", "cover_image_url": "https://...", "genres": ["Adventure"],
              "rating": 8.9, "episode_count": 28, "studio": "Madhouse", "status": "RELEASING",
              "year": "2023", "anilist_id": 51140, "trailer_id": "abc", "trailer_site": "youtube",
              "trailer_thumbnail": "https://...", "created_at": "..." } ],
  "total": 128, "rawTotal": 131, "limit": 50, "offset": 0 }
```

Rows are de-duplicated by identity — `anilist:<id>` when present, otherwise the normalized
title — keeping the row with more metadata. `total` is the page after de-duplication;
`rawTotal` is the unpaginated table count, so a client can tell the two apart instead of
silently losing cards.

### `GET /api/shows/{id}`

Returns the show object **directly** (no envelope, because the desktop player reads it that
way) with `episodes` attached:

```json
{ "id": "uuid", "title": "Frieren: Beyond Journey's End", "...": "show fields",
  "episodes": [ { "id": "uuid", "season_number": 1, "content_type": "tv",
                  "episode_number": 1, "title": null, "thumbnail": "https://...",
                  "duration": null, "created_at": "..." } ] }
```

Episodes are ordered by season then number, de-duplicated on `(season, content_type, episode)`
— the combination a re-scan can produce twice — and rows with a null episode number are
dropped rather than merged at 0. A fractional number (5.5 for a special) is preserved.
`thumbnail` falls back to the show's cover, which is the shape the clients already render.
`file_path` and `bucket_name` are never serialized.

404: `Show not found.`

---

## Authentication

See [Authentication](02-authentication.md) for the flows.

### `POST /api/auth/signup`

```json
{ "email": "nikhil@example.com", "password": "at-least-8-chars", "username": "optional" }
```

201 → `{ "access_token": "as_...", "token_type": "Bearer", "expires_at": "...", "user": {...} }`
plus the session cookie. `username` defaults to the email's local part; the avatar defaults to
a generated placeholder. 409 when the email is taken, 400 on validation, 403 when `ALLOW_SIGNUP=false`.

### `POST /api/auth/login`

```json
{ "email": "...", "password": "...", "remember_me": false }
```

200 as above. 401 `email or password is incorrect` (identical for unknown address and wrong
password), 423 when the account is locked.

### `POST /api/auth/logout`

Revokes the calling session and clears the cookie. Safe to call anonymously.

### `GET /api/auth/me`

200 → `{ "user": {...}, "session_kind": "web" | "desktop" }`.

### `GET /api/auth/google/url?redirect=<same-origin-url>`

503/501 `oauth_not_configured` when the provider is not configured; otherwise
`{ "url": "https://accounts.google.com/o/oauth2/v2/auth?...", "state": "<signed state>" }`.
`redirect` is honoured only if it names a `FRONTEND_URL` or `APP_LINK_ORIGINS` origin.

### `GET /api/auth/google/start?redirect=<app-link-url>`

Sets the same browser-nonce cookie and answers `302` to the provider. This is the entry point a
native client opens in a Custom Tab: `/google/url` sets the cookie in the caller's jar, which is not
the browser that later receives the callback.

### `GET /api/auth/google/callback?code&state`

Browser-only. Sets the session cookie and redirects to the configured return target with
`?auth_code=...`, or to `?auth=failed` / `?auth=cancelled`.

### `POST /api/auth/consume-code`

`{ "code": "..." }` → 200 with a session, exactly like login. Two-minute lifetime, single use.

### `POST /api/auth/password/change`

`{ "current_password": "...", "new_password": "..." }`. Revokes every other web session.

### `POST /api/auth/password/forgot` / `POST /api/auth/password/reset`

`{ "email": "..." }` always answers 202 with the same message; the reset token is logged for
the operator to deliver (no mail transport). `{ "token": "...", "new_password": "..." }`
completes it and revokes all sessions.

### `GET /api/auth/sessions`

Lists live credentials for the account:

```json
{ "web_sessions": [ { "id": "uuid", "device_name": "web", "user_agent": "...",
                      "ip_address": "203.0.113.4", "remember_me": true,
                      "expires_at": "...", "last_used_at": "..." } ],
  "desktop_devices": [ { "id": "uuid", "session_id": "...", "device_id": "hostname",
                         "device_name": "Animind Qt Desktop", "created_at": "...",
                         "last_seen_at": "...", "expires_at": "..." } ],
  "current_session": "uuid", "current_device": "" }
```

### `DELETE /api/auth/sessions/{id}`

Revokes one web session or desktop device by id.

### Desktop pairing

| Endpoint | Auth | Body | Response |
| --- | --- | --- | --- |
| `POST /api/auth/desktop/bridge-token` | session | – | `{ token, session_id, user_id, email, expires_in }` |
| `POST /api/auth/desktop/exchange` | session or bridge JWT | `{ deviceId?, deviceName? }` | `{ token_type, access_token, refresh_token, session_id, expires_in, access_token_expires_at, refresh_token_expires_at }` |
| `POST /api/auth/desktop/refresh` | none | `{ refresh_token }` | same shape; the presented refresh token stops working |
| `POST /api/auth/desktop/revoke` | optional bearer | `{ refresh_token? }` and/or bearer access token | `{ success: true }`, 404 when nothing matched |

`deviceId` and `deviceName` are trimmed to 128 characters. `POST /api/auth/desktop/bridge-token`
answers 409 when called with a desktop token.

---

## Playback

### `GET /api/episodes/{id}/stream-ticket`

Session required. Query: `clientType=desktop|native` (or a matching user agent), and `at=<audio
stream index>` for a browser that picked a non-default track.

```json
{ "url": "/api/episodes/<id>/stream?st=<ticket>&at=2", "expiresIn": 14400,
  "hlsRequired": true, "clientType": "web", "message": "Direct stream available." }
```

`hlsRequired` is `true` only for a local file a browser cannot play progressively (anything
that is not `.mp4`/`.m4v`/`.webm`). For the native client the ticket binds **no** audio track:
mpv selects tracks from the source container itself, so the original file must be delivered.

### `GET /api/episodes/{id}/stream`

Authenticated by a valid `?st=` ticket **or** by a session credential.

| Mode | Behaviour |
| --- | --- |
| S3 | `302` to a presigned URL. With `Accept: application/json`, answers `{ "type": "redirect", "url": "...", "expiresIn": 14400 }` instead. |
| Local | Byte-range passthrough of the file (or of the cached audio variant when a browser asked for one). |

Range handling: single ranges only (`bytes=a-b`, `bytes=a-`, `bytes=-n`). A response is `206`
with `Content-Range`, `Accept-Ranges: bytes` and a chunk cap of 8 MiB for browsers / 32 MiB for
the native player; an unsatisfiable range answers `416` with `Content-Range: bytes */size`.
Multi-range requests are refused rather than silently truncated to the first span.

### `GET /api/episodes/{id}/subtitles`

Session required. `tracks` is always present, and each `content` is a complete WebVTT document
because clients parse cues themselves rather than using a `<track>` element.

```json
{ "episodeId": "uuid", "tracks": [ { "id": "embedded-English", "label": "English",
    "language": "English", "content": "WEBVTT\n\n00:00:01.000 --> 00:00:04.000\n..." } ] }
```

Sources, in order: the canonical `Subtitles/[Season NN/]Episode NN/` directory, its legacy
spellings, then matching sidecar files beside the video. `.srt` is converted; ASS/SSA goes
through the converter that drops signs, karaoke and credit styles. In S3 mode with no extracted
subtitles the answer is an empty list, not an error.

### `GET /api/episodes/{id}/audio-tracks`

Session required, local mode only (S3 answers `{ "tracks": [], "mode": "presigned" }` because
the browser demuxes the file itself).

```json
{ "episodeId": "uuid", "tracks": [ { "id": "audio-3", "label": "Japanese #3 (OPUS / 2g)",
    "language": "Japanese", "streamIndex": 3, "codec": "opus", "browserSupported": false,
    "cached": true, "cacheMode": "transcode" } ] }
```

`browserSupported: false` with `cached: false` is the "first play will take a moment" signal the
site shows; the server builds a cached M4A variant on the first request for that track.

### HLS (local mode, browser playback)

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `POST /api/episodes/{id}/hls-session` | session | `{ audioTrackIndex?, startTime? }` → `{ sessionId, playlistUrl }`. Blocks until the first segment exists. 400 in S3 mode. |
| `GET /api/hls/{sessionId}/playlist.m3u8` | none | Segment lines are rewritten to file names so they resolve against this server. Before the first segment: a valid empty `EVENT` playlist. |
| `GET /api/hls/{sessionId}/{segment}` | none | `video/mp2t`, `no-cache`. Name must match `seg\d{5}\.ts`. |
| `POST /api/hls/{sessionId}/seek` | session | `{ "time": 742.5 }` restarts the remux at that offset. |
| `POST /api/hls/{sessionId}/heartbeat` | session | Keeps an idle session alive; returns `{ alive, stats }`. |
| `DELETE /api/hls/{sessionId}` | session | Kills the process and removes the directory. |

`audioTrackIndex` is genuinely optional: clients omit the key entirely on first play, which
means "default track". Capacity exhaustion answers 429 `hls_capacity`.

---

## User data

All under `/api/me`, session required, and every statement is filtered by the session's user
id — no endpoint accepts a `user_id` from the client.

### Profile

| Endpoint | Body / Result |
| --- | --- |
| `PATCH /api/me` | `{ username?, avatar_url? }` → `{ user }`. Avatar must be an http(s) URL. Adding `email` requires `current_password`, lowercases and validates the address, answers `409` when another account owns it and `400 {code:"password_not_set"}` for a Google-only account, then revokes every **other** web session. No verification mail is sent. |
| `DELETE /api/me` | Deletes the account and cascades every owned row and session. |

### Watchlist

Rows keep the existing client shape: `anime_id` is an AniList id (text) and `anime_data` is the
client's own show object, stored and returned verbatim so a card rebuilds without a catalog
lookup.

| Endpoint | Notes |
| --- | --- |
| `GET /api/me/watchlist` | **Top-level array** of rows (`id`, `user_id`, `anime_id`, `anime_data`, `status`, `created_at`, `updated_at`). Newest first. The desktop client requires a bare array. |
| `PUT /api/me/watchlist/{animeId}` | `{ anime_data, status?, anime_id? }`; upsert. `status` ∈ `Watching`, `Completed`, `Plan to Watch`, `Dropped` (default `Plan to Watch`). A body `anime_id` that disagrees with the path is a 409. |
| `PATCH /api/me/watchlist/{animeId}` | `{ status }` only. 404 when absent. |
| `DELETE /api/me/watchlist/{animeId}` | 404 when absent. |

`PUT` is idempotent because clients update optimistically and never roll back.

### Progress and history

| Endpoint | Notes |
| --- | --- |
| `GET /api/me/progress?anime_id=&episode_index=` | `{ timestamp, anime_id, episode_index }`. No row is `0`, not 404 — the player asks before playback and expects a number. |
| `PUT /api/me/progress` | `{ anime_id, episode_index, timestamp }` → `{ success }`. Written by a client timer every 5 s during playback. |
| `GET /api/me/progress/recent?limit=20` | `{ items: [{ anime_id, episode_index, timestamp, updated_at }] }`. |
| `GET /api/me/history?limit=50` | **Top-level array** of continue-watching rows for the desktop player. |
| `PUT /api/me/history` | `{ show_title, episode_label?, anilist_id?, thumbnail_url?, progress_pct }`; upsert keyed on show + episode label. |

`episode_index` is the client's positional index into its episode list, retained for
compatibility with rows that already exist; `progress_pct` is clamped to 0-100.

### Preferences

| Endpoint | Notes |
| --- | --- |
| `GET /api/me/preferences` | The stored JSON document, or `{}` when none. |
| `PUT /api/me/preferences` | Replaces it (max 64 KiB). Volume, speed, subtitle and audio choices, so they follow the account instead of one browser profile. |

---

## Library indexing

### `POST /api/admin/scan` (admin, JSON)

Runs a scan synchronously and returns the scanner result:

```json
{ "scanned": 128, "inserted": 126, "updated": 0, "removed": 2,
  "errors": [], "durationMs": 4210, "prunedEpisodes": 2, "prunedShows": 0,
  "subtitleFiles": 14 }
```

409 `scan_in_progress` when a scan is already running. Audio prewarm runs afterwards in the
background. The admin console uses the asynchronous `POST /admin/library/scan` instead and
shows the scanner status while it runs; see [Admin console](12-admin-panel.md).

The old website's `POST /api/rescan` endpoint was removed. It returned the whole library in a
camelCase shape that no shipped client calls.

### `POST /api/webhooks/storage`

Header `x-webhook-secret: $WEBHOOK_SECRET`. Answers `{ "received": true }` immediately and
scans in the background, because a bucket that waits would retry the notification. 401 on a
missing or wrong secret.

### `POST /api/anilist`

`{ query, variables? }` forwarded to AniList's GraphQL endpoint, which a browser cannot call
directly (no CORS headers), and which the native clients should not call directly either.
Read-only: a document containing `mutation` or `subscription` —
including one behind a `#` comment — is refused with 400, and `query` is capped at 10,000
characters. Upstream status and body are passed through; an unreachable AniList is 502
`anilist_unreachable`.

A repeated read-only document is answered from the shared store for
`CACHE_METADATA_TTL_HOURS`, and `X-Cache: HIT|MISS` says which happened. Only a `200` carrying
`data` and no `errors` is stored, so a rejection or a truncated body is retried rather than
remembered. Clients that ignore the header are unaffected.

---

## Watch parties

### `GET /api/syncplay/rooms/{code}`

Session required. Lets a client validate a code before asking the user to open the matching
episode:

```json
{ "code": "ABCDE", "episodeId": "uuid", "hostUserId": "uuid", "hostDisplayName": "nikhil",
  "status": "active", "currentTime": 412.5, "isPlaying": true, "participantCount": 3 }
```

404 `Room not found.` for a code that never existed, 410 `room_ended` for one that was live and
has since ended. The socket protocol is documented in [Watch parties](06-watch-parties.md).

---

## Torrent search and downloads

Every route below requires a session and is only served in local storage mode — in `s3` mode the
download routes answer 501 `downloads_unavailable`. Limits are per client IP per minute.

| Endpoint | Limit | Notes |
| --- | --- | --- |
| `GET /api/torrents/search` | 30 | `q` (2–120 chars, required), `season`, `episode`, `resolution`, `codec`, `minSeeders`, `batch=any\|only\|exclude`, `sort=relevance\|seeders\|size\|date`, `limit ≤ 100` |
| `POST /api/downloads` | 10 | `{ "resultId": "opaque" }`; needs `can_download` or admin |
| `GET /api/downloads?limit=&all=` | 120 | own jobs newest first; `all=true` is admin-only and lists every account |
| `GET /api/downloads/{id}` | 120 | one job with per-file progress; a foreign job is 404, like a missing one |
| `PATCH /api/downloads/{id}` | 60 | `{ "action": "pause"\|"resume" }` or `{ "fileIndexes": [...] }` while `awaiting_selection` |
| `DELETE /api/downloads/{id}?deleteFiles=` | 60 | cancel/remove; needs `can_download` or admin; `deleteFiles=true` also drops data and rescans |
| `GET /api/downloads/events` | 10 | SSE stream: snapshot, live job events, 15 s heartbeat |
| `GET /api/search/trending` | 60 | top 10 normalized queries of the last 7 days |
| `PATCH /api/admin/users/{id}` | 60 | admin; body may set `is_admin` and/or `can_download` |

`POST /api/downloads` resolves the opaque search id server-side: the magnet never reaches a
client. Responses and error codes:

- 201 returns the job: `{ id, resultTitle, infoHash, state, progressBytes, sizeBytes, progressPct,
  downKbps, upKbps, peers, etaSec, error, showId, createdAt, updatedAt, completedAt,
  files: [{ index, path, sizeBytes, selected, progressBytes }] }` (camelCase; the REST read
  merges the live engine view when the job is running in this session).
- 410 `result_expired` — the search result's cache entry is gone.
- 403 `download_forbidden` — no `can_download` entitlement, or the quota is already fully used.
- 409 with `existingJobId` — the same torrent is already queued or downloading; finished and
  cancelled rows do not count.
- 400 `job_too_large`, 507 `insufficient_disk`, 501 `downloads_unavailable` /
  `indexer_not_configured`, 429 on limit refusals.
- `GET /api/torrents/search` returns `{ data: [{ id, infoHash, title, sizeBytes, seeders,
  leechers, grabs, publishedAt, indexer, category, parsed: { show, season, episodeStart,
  episodeEnd, isBatch, resolution, source, videoCodec, bitDepth, audioCodecs, dualAudio, subs,
  hdr, releaseGroup }, health, trusted }], total, cached, queryUsed }` — no magnet field exists
  on the wire shape.

SSE frames are `event: snapshot` (one per known job on connect) and `event: job` (live), each
carrying `{ id, state, progress, bytesDone, sizeBytes, downKbps, upKbps, peers, etaSec, showId,
error? }`.

The full operational story — setup, quotas, the `.incoming` directory, imports — lives in
[Downloads](11-downloads.md).

---

## Administration

| Endpoint | Notes |
| --- | --- |
| `GET /api/admin/users?limit=&offset=` | Array of `{ id, email, username, avatar_url, is_admin, can_download, created_at, updated_at }`, newest first. Every account appears, including ones that never signed in. |
| `PATCH /api/admin/users/{id}` | `{ "is_admin": true\|false }` and/or `{ "can_download": true\|false }` (the torrent-download entitlement). Refuses self-demotion. |
| `DELETE /api/admin/shows/{id}` | Deletes the show; its episodes cascade. |
| `POST /api/admin/scan` | Raw scan result: `{ scanned, inserted, updated, removed, errors, durationMs, prunedEpisodes, prunedShows, subtitleFiles }`. |
| `GET /api/admin/metrics` | Real counts: shows, episodes, users, active watch parties, socket peers/rooms, storage mode, uptime, HLS session stats. |
| `POST /api/admin/library/cleanup` | Runs the audio-variant TTL sweep now (local mode only). |

Admin routes require `RequireAuth` then `RequireAdmin`; the flag is read from the account row
per request.

Related: [Architecture](01-architecture.md), [OpenAPI](openapi.yaml).
