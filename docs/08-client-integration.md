# 8. Client integration

What each native client needs. Paths, field names and socket payloads are unchanged wherever a
shipped client parses them, so client changes stay confined to authentication and to the data
that used to be fetched from a database directly. The old React website was retired; the only
website is the [admin console](12-admin-panel.md).

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
   those claims. The player's own code needs no change. The backend now serves the handoff page
   itself at `/desktop-auth`, so set `ANIMIND_SITE_URL` to this API origin (for example
   `http://127.0.0.1:3001`); the page signs the browser in, shows a **Pair this desktop**
   confirmation, and redirects to `http://127.0.0.1:27182/auth?token=<jwt>&sessionId=<session_id>`.
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
   It also never calls the admin, scan, subtitles or audio-track endpoints.

## Admin console

The website is now the server-rendered admin console at `/admin`; it is embedded in the Go
binary, so there is no React/Clerk/Supabase migration to perform. The old React site is not
part of this repository and should be retired from the deployment.

- Sign in at `/admin/login` with an account whose `is_admin` flag is true. The form posts to
  the same auth service as the API and sets the HttpOnly session cookie.
- Every mutating form carries a double-submit CSRF token. The console sends a CSP with no
  script source, so do not add inline scripts to it.
- Account pages show live app/browser sessions and desktop devices; both can be revoked. The
  library page runs scans and shows the last result.
- The JSON admin API (`/api/admin/*`) remains available to native clients; the console is a
  thin HTML layer over the same store.

If you keep a separate web client, it must use `Authorization: Bearer` or same-origin
`credentials: 'include'`; see [Authentication](02-authentication.md). The API sends
`X-Frame-Options: DENY`, and serves HTML only under `/admin` and `/desktop-auth`.

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
