# 8. Client integration

What each client needs, and exactly what changes when Clerk is removed. Paths, field names and
socket payloads are unchanged wherever a shipped client parses them, so the diff on the client
side is confined to authentication and to the data it used to fetch from a database directly.

## Desktop player (Qt/QML + libmpv)

The player talks to five endpoints. All five are implemented with the same paths, methods and
field names:

| Call | Notes |
| --- | --- |
| `POST /api/auth/desktop/exchange` | `Authorization: Bearer <credential from the browser handoff>`, body `{ deviceId, deviceName }` |
| `POST /api/auth/desktop/refresh` | `{ refresh_token }`, no Authorization header |
| `POST /api/auth/desktop/revoke` | `{ refresh_token }` and/or bearer access token; response may be ignored |
| `GET /api/shows/{id}` | `User-Agent: Animind-Qt/1.0`; response is the show object **directly** with `episodes[]` |
| `GET /api/episodes/{id}/stream-ticket?clientType=native` | only `url` is read |

Behaviour the backend must keep honouring, taken from the client's source:

- **`access_token_expires_at` wins over `expires_in`.** The player parses it with `Qt::ISODate`
  when it is a string, so it is always sent ISO-8601; `expires_in` is the fallback.
- **Refresh is scheduled for `expiry - now - 30s`, minimum 15 s.** The access token TTL
  (15 minutes by default) is well above that floor.
- **There is no 401-retry.** The player only checks its local clock before a request; a request
  that fails because the server revoked the session surfaces as an empty result with a log line.
  So this backend never reports a longer validity than it will honour: the token expiry, the
  session row's `access_token_expires_at`, and the signature's `exp` are set from one value.
- **Playback URLs must be self-authenticating.** The player passes the URL straight to
  `mpv loadfile` with no `http-header-fields`, no cookie and no user agent, and
  `ytdl=no`, so ffmpeg's HTTP protocol fetches it. The ticket in `?st=` is what authorizes those
  bytes. Its distinctive `User-Agent` appears only on the two GETs, never on the media request, so
  native mode is read from the ticket.
- **`at` is never sent by this client** (`audioTrackIndex` is hard-coded `-1` at its only call
  site), and a native ticket binds no track: the player selects audio and subtitles from the
  delivered container. Serving a remuxed variant would drop them, so `cm="n"` returns the original
  file.
- **`GET /api/shows/{id}` is expected unwrapped.** Any `data`/`show` envelope would leave every
  field read by the client undefined.
- Extra response fields are tolerated; unknown ones on the ticket are ignored.

### What must change in the player

Three of its five dependencies are gone:

1. **The browser handoff token.** The player decodes the token it receives on
   `127.0.0.1:27182` as a JWT and reads `sub`, `sid`, `exp`. That token was a Clerk session JWT;
   now it is the bridge JWT from `POST /api/auth/desktop/bridge-token`, which carries exactly
   those claims. The player's own code needs no change — the site's `/desktop-auth` page does:
   request a bridge token and navigate to
   `http://127.0.0.1:27182/auth?token=<jwt>&sessionId=<session_id>`.
2. **Clerk FAPI calls.** `GET {fapi}/v1/client` and
   `POST /v1/client/sessions/{id}/tokens/supabase` are gone. The email is in the bridge JWT; there
   is no Supabase token to mint.
3. **PostgREST reads and writes.** `watchlist` and `watch_history` were fetched from
   `ANIMIND_SUPABASE_URL` with an anon key plus a Clerk-minted JWT. With Clerk gone that path
   cannot authenticate, so these become API calls:

   | Was | Now |
   | --- | --- |
   | `GET /rest/v1/watchlist?user_id=eq.<uid>&order=created_at.desc` | `GET /api/me/watchlist` (bearer desktop token) |
   | `POST /rest/v1/watchlist` with `Prefer: resolution=merge-duplicates` | `PUT /api/me/watchlist/{anime_id}` |
   | `PATCH /rest/v1/watchlist?user_id=…&anime_id=…` | `PATCH /api/me/watchlist/{anime_id}` |
   | `DELETE /rest/v1/watchlist?user_id=…&anime_id=…` | `DELETE /api/me/watchlist/{anime_id}` |
   | `POST /rest/v1/watch_history` | `PUT /api/me/history` |
   | `GET /rest/v1/watch_history?order=last_watched.desc` | `GET /api/me/history` |

   The row shapes are deliberately the same: watchlist rows carry `user_id`, `anime_id`,
   `anime_data` (the whole show object) and `status`, and both lists are returned as **top-level
   arrays**, because the client guards on `doc.isArray()`. History rows keep the seven keys the
   player already writes — `user_id`, `show_title`, `episode_label`, `thumbnail_url`, `anilist_id`,
   `last_watched`, `progress_pct`. Only the URL, the header (`Authorization: Bearer adk_...`
   instead of `apikey`), and the removal of `user_id` from the request differ: the server derives
   it from the token.

   That last point is a security fix, not a convenience. Those writes sent a client-chosen
   `user_id`.

4. **`ANIMIND_BACKEND_URL` is live.** It is read nowhere in the current player: the base URL is a
   compiled-in constant (`kDefaultBackendBaseUrl`), so pointing a build at this backend means
   changing that constant or fronting it at the same hostname. Nothing else in the environment file
   is read either.
5. The player has **no watch-party code** (QtWebSockets is not even linked), so nothing to do there.
   It also never calls rescan, admin, subtitles or audio-track endpoints.

## Website (React SPA)

### Authentication

| Was (Clerk) | Now |
| --- | --- |
| `<ClerkProvider publishableKey=…>` + `<SignIn/>`/`<SignUp/>` components | `POST /api/auth/login` and `/api/auth/signup`; render your own form |
| `useAuth().getToken()` for API calls | the session token: `Authorization: Bearer as_...`, or nothing at all if you let the HttpOnly cookie ride (`credentials: 'include'`) |
| `useUser()` for id, name, avatar | `GET /api/auth/me` → `{ user: { id, email, username, avatar_url, is_admin } }` |
| `user.publicMetadata.isAdmin === true` | `user.is_admin`, from the same endpoint |
| `clerkUser.delete()` | `DELETE /api/me` |
| `getToken({template: 'supabase'})` for direct DB access | no equivalent — see the data table below |
| `getToken({template: 'desktop', skipCache: true})` for pairing | `POST /api/auth/desktop/bridge-token` |
| `openUserProfile()` | `PATCH /api/me` |

Google sign-in: `GET /api/auth/google/url?redirect=<same-origin>` → navigate → the callback
returns to the API and redirects to `redirect?auth_code=...` → `POST /api/auth/consume-code`
exchanges the code for a session. A one-time code is used rather than putting the session token in
a URL, where history and `Referer` would keep it. If you prefer the cookie alone, nothing is needed
after the redirect: the callback already set it.

### Data that was direct-to-database

Those calls sent a client-supplied `user_id` and relied on row-level security keyed to an external
JWT. Both are gone; the session decides ownership.

| Was | Now |
| --- | --- |
| `from('profiles').upsert({ id, username, avatar_url, updated_at })` on every login | nothing — accounts are created by signup, and `PATCH /api/me` updates them. There is no `profiles` bootstrap to satisfy an FK. |
| `from('watchlist').select('*').eq('user_id', uid)` | `GET /api/me/watchlist` |
| `.upsert({ user_id, anime_id, anime_data, status }, { onConflict: 'user_id, anime_id' })` | `PUT /api/me/watchlist/{anime_id}` |
| `.update({ status }).eq('user_id', uid).eq('anime_id', id)` | `PATCH /api/me/watchlist/{anime_id}` |
| `.delete().eq('user_id', uid).eq('anime_id', id)` | `DELETE /api/me/watchlist/{anime_id}` |
| `from('progress').select('timestamp')…maybeSingle()` | `GET /api/me/progress?anime_id=&episode_index=` → `{ timestamp }` (0 when absent) |
| `from('progress').upsert({ user_id, anime_id, episode_index, timestamp })` | `PUT /api/me/progress` |

`anime_data` is stored and returned verbatim, so `map(item => ({ ...item.anime_data, userStatus:
item.status }))` keeps working unchanged. Keep `PUT` idempotent in your optimistic UI: the server is
now the truth source and will not roll back.

The resume arithmetic the site does today is worth keeping server-side in mind: `resumeTime > 5 ?
resumeTime - 2 : 0` (a 2 s pre-roll, ignoring tiny resumes) and adding the HLS start offset back
before saving. Those are client choices this backend deliberately does not replicate, so a saved
position is exactly what the client sent.

### New endpoints available to the site

`GET /api/me/progress/recent` (there was no resume rail because no such query existed),
`GET`/`PUT /api/me/preferences` (volume, speed, subtitle and audio choices are the only settings
the site had in `localStorage`; the audio choice was written and never read back),
`GET /api/auth/sessions` plus `DELETE /api/auth/sessions/{id}`, and `POST /api/hls/{id}/heartbeat`.

`GET /api/shows` now accepts `q` and `genre`, and its `limit` goes to 200 — the catalog is no
longer silently capped at whatever one unpaginated call returned. Episode rows carry `thumbnail`
(the show cover when there is no per-episode artwork) and `season_number`, so seasons no longer
need to be fabricated by chunking 24 episodes.

`GET /api/admin/metrics` returns real counts; the dashboard's "Server Load 34%", "API Latency
42ms" and hard-coded log lines have no server counterpart and should be removed or fed from it.

### Playback and subtitles

Unchanged: `hlsRequired`/`clientType` device negotiation, `playlistUrl`, the ticket URL shape, and
`{ tracks: [{ id, label, language, content }] }` with `content` a full WebVTT document — still
parsed by your `parseVTT`, not a `<track>` element. Audio-track switching still round-trips
`streamIndex` through `?at=` and re-fetches the stream, and the "first play may take a minute"
notice still keys on `!cached && !browserSupported`.

`expiresIn` on a ticket remains unused by the site; with `STREAM_TICKET_TTL_SECONDS` at 4 hours a
long episode is not at risk, and the heartbeat endpoint covers an HLS gap.

### Watch parties

Nothing to change. Same path (`/api/socket.io`), same `auth: { token }` (send the session token
instead of a Clerk token), same event names and payloads. `transports: ['websocket','polling']`
works: WebSocket is native here and long-polling with upgrade is supported.

`GET /api/syncplay/rooms/{code}` can validate the join bar's code before asking the user to open
the matching episode, which the UI could not do before.

### Infrastructure notes

- **CSP**: the site's `connect-src`, `media-src` and `default-src` must list the API host for both
  `https://` and `wss://`.
- The session cookie is `HttpOnly` and `SameSite=Lax`. `SameSite=Lax` does not attach it to the
  SPA's cross-site `fetch` — those requests must send the bearer header, or use
  `credentials: 'include'` with the site and API on one registrable domain.
- The API sends no `frame-ancestors`/CSP of its own beyond `X-Frame-Options: DENY`: it serves no
  HTML. The page that embeds the YouTube trailer iframe and the player is yours.

## Curl cookbook

```bash
API=http://localhost:3001
KEY=...   # from the login response

curl -s $API/health
curl -s -XPOST $API/api/auth/signup -H 'content-type: application/json' \
  -d '{"email":"nikhil@example.com","password":"secret-ish","username":"nikhil"}'
curl -s -XPOST $API/api/auth/login -H 'content-type: application/json' \
  -d '{"email":"nikhil@example.com","password":"secret-ish","remember_me":true}'

curl -s $API/api/shows | head -c 400
curl -s -H "Authorization: Bearer $KEY" $API/api/auth/me
curl -s -H "Authorization: Bearer $KEY" $API/api/me/watchlist

# pair a desktop device: bridge token, then exchange
B=$(curl -s -XPOST -H "Authorization: Bearer $KEY" $API/api/auth/desktop/bridge-token \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
curl -s -XPOST $API/api/auth/desktop/exchange -H "Authorization: Bearer $B" \
  -H 'content-type: application/json' -d '{"deviceName":"Animind Qt Desktop","deviceId":"vps"}'

# playback
T=$(curl -s -H "Authorization: Bearer $KEY" $API/api/episodes/$EP/stream-ticket)
curl -s -o /dev/null -w '%{http_code}\n' "$API$(echo $T | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')"

# save a resume position
curl -s -XPUT -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"anime_id":"153564","episode_index":3,"timestamp":412.5}' $API/api/me/progress
```

Related: [Authentication](02-authentication.md), [API reference](03-api-reference.md),
[Streaming](05-streaming.md).
