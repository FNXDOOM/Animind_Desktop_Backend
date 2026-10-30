# 2. Authentication

This backend is the identity provider. There is no Clerk, no Supabase GoTrue, no external
session service, and no JWT signed by somebody else's key. Accounts, credentials, sessions
and admin rights live in Postgres and are decided here.

## Token families

| Family | Format | Storage | Lifetime | Verifier |
| --- | --- | --- | --- | --- |
| Site session | `as_` + 32 random bytes, base64url | SHA-256 hash in `animind_web_sessions` | `WEB_SESSION_TTL_HOURS`, sliding; `REMEMBER_ME_TTL_DAYS` when requested | `authenticateWeb` |
| Desktop access | `adk_` + base64url(`v1.<user>.<session>.<exp>`) + `.` + HMAC-SHA256 | SHA-256 hash in `desktop_auth_sessions` | `DESKTOP_ACCESS_TOKEN_TTL_SECONDS` (15 min) | `authenticateDesktop` |
| Desktop refresh | `adr_` + 48 random bytes | SHA-256 hash, rotated on each use | `DESKTOP_REFRESH_TOKEN_TTL_DAYS` (30 d) | `DesktopRefresh` |
| Bridge JWT | HS256 JWT, `aud=animind-desktop-bridge`, 60 s | not stored | 60 s | `VerifyBridgeToken` |
| Playback ticket | base64url(JSON) + `.` + HMAC-SHA256 | not stored | `STREAM_TICKET_TTL_SECONDS` (4 h, min 120 s) | `VerifyStreamTicket` |
| Reset / one-time code | 32 random bytes | SHA-256 hash in `animind_auth_tokens` | 60 min / 2 min, single use | `ConsumeAuthToken` |

All of them are verified with one secret, `DESKTOP_TOKEN_SIGNING_KEY`. Rotating it
invalidates every desktop session and outstanding ticket at once — which is the correct
behaviour for a leaked key, and the reason it is a required startup value.

### Why a desktop access token is signed *and* stored

`internal/auth/tokens.go` issues a self-contained HMAC token so a bad signature is rejected
without touching the database, and `internal/auth/service.go` then requires a matching live
row. The signature keeps the cheap path cheap; the row is what makes revocation instant
instead of waiting for expiry.

```
payload = "v1." + userID + "." + sessionID + "." + expUnixSeconds
token   = "adk_" + base64url(payload) + "." + base64url(HMAC-SHA256(key, payload))
```

That layout is byte-for-byte what the shipped Qt player already parses, and its response
fields (`access_token`, `refresh_token`, `session_id`, `expires_in`,
`access_token_expires_at`) are unchanged. The player prefers `access_token_expires_at` when
it is a string, so it is always sent as ISO-8601.

### Why a bridge JWT exists at all

The desktop player opens a browser for sign-in, then listens on `127.0.0.1:27182` and
**decodes whatever token it receives as a JWT**, reading the claims `sub`, `sid` and `exp`
before it posts the token to `/api/auth/desktop/exchange`. A opaque token would break that
decode step. So `POST /api/auth/desktop/bridge-token` (called by the logged-in site) issues
an HS256 JWT carrying exactly those claims, and the exchange endpoint accepts it.

The verifier refuses anything whose header `alg` is not `HS256`, and requires
`aud=animind-desktop-bridge` — a token minted for another purpose, or with `alg: none`, does
not pass. See `internal/auth/jwt.go`.

## Passwords

argon2id, encoded in the PHC string format:

```
argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 derived key>
```

Defaults are the OWASP second choice (`ARGON2_TIME=3`, `ARGON2_MEMORY_KIB=65536`,
`ARGON2_THREADS=4`), overridable by environment. A 16-byte random salt per password means two
users with the same password share nothing comparable in the database.

Because the parameters are embedded in each hash, verification re-derives with the parameters
the hash was made with, and reports whether the stored hash is weaker than the current
settings. On a successful login with a weaker hash, `Login` upgrades it in place. That is what
makes raising the cost factors a non-event: no rehash pass, no forced resets.

Rules enforced: 8-256 characters. No composition rules — they push people toward predictable
substitutions and are trivially bypassed server-side anyway.

## Login failure handling

- Unknown email and wrong password produce the same response and take similar time: when the
  account is missing, `burnTime` runs an argon2id verification against a fixed placeholder so
  the endpoint does not answer faster for a non-existent account.
- `failed_login_count` and `locked_until` are on the user row. After `AUTH_MAX_FAILED_LOGINS`
  failures the account answers **423** for `AUTH_FAILED_LOGIN_LOCKOUT_MINUTES`. A successful
  login clears both.
- Reset requests always answer 202 with the same message. The generated token is written to
  the server log, because no mail transport is configured here and inventing one is out of
  scope; an operator delivers it.

## Sessions

Site sessions are opaque (`as_...`), delivered two ways because the two site integration
styles need different handling:

- `Set-Cookie: animind_session=...; HttpOnly; SameSite=Lax; Secure (in production); Path=/`
- `access_token` in the login response body, for a SPA that sends `Authorization: Bearer`.

`credential()` in `internal/httpx/server.go` accepts either. Revoking the row signs the
browser out on its next request.

Sliding expiry is rate-limited: `last_used_at` and `expires_at` are rewritten at most once a
minute per session (`touchInterval`), otherwise authentication would become the most
expensive part of every request.

That same interval bounds the **credential cache**. A resolved token is kept in the shared
store under the SHA-256 hash the database already stores, for
`min(CACHE_SESSION_TTL_SECONDS, touchInterval, the row's own expiry)`, and every revoke,
rotation, password change, profile edit and admin-flag write evicts the account's entries.
Consequences worth knowing:

- A miss costs the two SELECTs it always cost. A store that cannot be answered is a miss, not
  a 401 — losing Redis costs latency, never access.
- Revocation is immediate on every path that names the account. What no cache can erase is an
  entry written by a resolution already in flight when the revocation landed, so the ceiling
  on that race is the TTL: one minute.
- The entry cannot outlive its row, so a session cannot be extended by not looking at the
  database.

Changing or resetting a password revokes **all** web sessions for that account, so a stolen
cookie cannot outlive the credential. `POST /api/auth/logout` revokes just the calling
session; `POST /api/auth/sessions` lists live sessions and desktop devices, and
`DELETE /api/auth/sessions/{id}` revokes one.

## Desktop pairing flow

```
1. Site, already logged in:  POST /api/auth/desktop/bridge-token   (Bearer as_...)
                            -> { token: <JWT>, session_id, expires_in: 60 }
2. Site navigates the desktop's loopback listener:
                            http://127.0.0.1:27182/auth?token=<JWT>&sessionId=<id>
3. Desktop decodes the JWT (sub, sid, exp), then:
                            POST /api/auth/desktop/exchange
                            Authorization: Bearer <JWT>
                            { "deviceId": "<hostname>", "deviceName": "Animind Qt Desktop" }
                            -> { token_type, access_token, refresh_token, session_id,
                                 expires_in, access_token_expires_at,
                                 refresh_token_expires_at }
4. Every API call:           Authorization: Bearer <access_token>
5. ~30 s before expiry:      POST /api/auth/desktop/refresh { refresh_token }
6. Sign out:                 POST /api/auth/desktop/revoke { refresh_token }
                             Authorization: Bearer <access_token>
```

`refresh` rotates both tokens in one statement, so the presented refresh token cannot be used
twice. `revoke` accepts either half of the pair and answers 404 only when neither matched.
A bridge JWT is minted only from a web session: pairing an already-paired desktop token would
bind two machines to one session, which is why `POST /api/auth/desktop/bridge-token` answers
409 for a desktop principal.

## Google sign-in

The authorization-code flow is spoken directly to Google's endpoints
(`internal/auth/google.go`), with no SDK: `GET /api/auth/google/url` builds the redirect with
a signed single-use `state`, and `GET /api/auth/google/callback` exchanges the code, reads the
userinfo profile, and provisions the account.

Account matching is by Google `sub` first, then by verified email. So a user who signed up
with a password and later clicks Google lands on **one** account, and the identity is attached
to it. An account created this way has `password_hash = NULL`, so password login for it is
refused with the same generic message rather than a hint about the provider.

After the callback the browser is redirected to a configured origin with `?auth_code=...`,
which the site redeems via `POST /api/auth/consume-code`. A one-time code is used rather than
the session token itself so no credential ever lands in history, a log, or a `Referer`. The
`redirect` parameter is only honoured when it names an origin from `FRONTEND_URL` or
`APP_LINK_ORIGINS`, which is what stops this being an open redirect.

A native client cannot use `GET /api/auth/google/url`: it sets the browser-nonce cookie in the
caller's cookie jar, which is the app, not the Custom Tab that later receives the callback.
`GET /api/auth/google/start` exists for that case — it sets the cookie and answers 302 to the
provider from inside the browser. `APP_LINK_ORIGINS` lists the HTTPS origins the callback may
return to; entries with a path, query, fragment, userinfo, wildcard or `http` scheme are dropped at
boot.

`GOOGLE_CLIENT_ID`/`SECRET` unset means `GoogleEnabled()` is false: the URL endpoint answers
501 with a `oauth_not_configured` code instead of redirecting to a broken flow.

## Authorization

Two roles, one bit: `is_admin` on `animind_users`. `RequireAuth` resolves the principal and
`RequireAdmin` refuses with 403 when the bit is clear. The admin flag is read from the
account row at request time, so a demotion takes effect on the next request rather than at
token expiry.

Self-demotion is refused: `PATCH /api/admin/users/{id}` cannot clear the caller's own flag,
which is how an admin panel locks everyone out.

The deleted-account path is server-side (`DELETE /api/me`, previously `clerkUser.delete()` in
the browser). Deleting a user cascades watchlist, progress, history, preferences, web
sessions, desktop sessions, linked identities and owned watch rooms;
`TestDeleteAccountCascadesEverything` asserts each table is empty afterwards.

## Migration from Clerk

There is no identity import path: Clerk user ids (`user_2abc...`) and Supabase auth rows have
no password or provider credential this backend can verify, and copying them would mean
trusting a third party's secret. Accounts here start empty.

- **Existing users** sign up again with the same email address, which creates a fresh account.
  Their watchlist and progress rows are keyed on the old id.
- **Existing watchlist/progress rows** are keyed on `user_id`, so a re-created account does not
  inherit them. The watchlist and progress *content* is keyed on AniList ids
  (`anime_id`, `anime_data`) which are identity-independent, so a re-add is the only data loss.
- **Admin rights** must be re-granted: `PATCH /api/admin/users/{id}`, or
  `update animind_users set is_admin = true where email_normalized = '...'`.
- **`profiles` is gone** — `animind_users` carries `username`, `avatar_url` and `is_admin`, so
  the bootstrap upsert the site used to run on every login is no longer needed.

See [Client integration](08-client-integration.md) for the corresponding client changes.

Related: [API reference](03-api-reference.md), [Security review](09-security.md).
