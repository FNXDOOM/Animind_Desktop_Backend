# 12. Admin console

The website is the admin console at `/admin`. The only other HTML page is the desktop pairing
handoff at `/desktop-auth` (see below); there is no public catalogue site. The admin console is
a server-rendered operator surface for the people who run the library. The Android app and the
desktop player are the user-facing clients.

## Shape

- The templates and stylesheet are embedded in the Go binary (`internal/httpx/adminui/`).
  There is no Node build, no separate static host and no second deployment.
- Every page is rendered from `html/template` with contextual escaping. The panel ships no
  JavaScript.
- `GET /admin` requires an administrator session. Anonymous visitors are redirected to
  `/admin/login`; a signed-in non-admin gets a 403 page.
- The root `/` redirects to `/admin`, so there is no leftover public site to maintain.

## Signing in

1. Sign in at `/admin/login` with an account whose `is_admin` flag is true. The console uses
   an email/password login; it deliberately does not offer Google sign-in, so promote an
   existing password account rather than a Google-only account.
2. The session uses the same HttpOnly cookie as the JSON API. In a Secure deployment it is
   named `__Host-animind_session`; the legacy `animind_session` name is cleared but never
   accepted, so an existing browser may need one sign-in after upgrading.
3. To promote the first operator, set the flag directly:

```sql
update animind_users set is_admin = true where email = 'operator@example.com';
```

The admin login does not create accounts. Mobile and desktop users register through the native
clients; administrators promote an existing account.

## Pages

### Overview

Account totals (admins, download entitlement), library counts, live app/browser sessions,
paired desktop devices, watch-party peers and rooms, the last scan result and server details.

### Accounts

A searchable, paginated account list with the number of live app/browser sessions and
desktop devices per account. Opening an account shows:

- Access toggles: **Administrator** and **Torrent downloads**. The signed-in operator cannot
  remove their own admin flag.
- Every live web/app session with a guessed platform label, device name, address, last-used
  time and expiry. Each row can be revoked.
- Every paired desktop device with its name, device id, pairing time, last seen time and
  expiry. Each row can be revoked.
- **Revoke all sessions** (web/app and desktop) and **Delete account**. Both require a
  confirmation page; deleting an account cascades its watchlist, progress, history, sessions
  and devices. The operator cannot delete their own account from the panel.

### Library

The scanner card reports whether a scan is running and the last result (files seen, indexed,
removed, pruned rows, duration, warnings, last error). A scan started here runs in the
background and the page refreshes while it is running. The page also lists indexed shows with
search and pagination, and can:

- run a scan (`POST /admin/library/scan`),
- clean the local audio-variant cache when `STORAGE_MODE=local`,
- delete a show after a confirmation page. Media files are not touched; a later scan can index
  them again.

Scanner status is per process: a restart starts with “no scan yet”, which is honest rather
than showing a stale result.

## Desktop pairing page

`/desktop-auth` is the one non-admin HTML page: the stock Qt player opens it in the system
browser, the page signs the user in, and a **Pair this desktop** button mints the 60-second
bridge JWT and redirects to the player's fixed loopback listener on `127.0.0.1:27182`. The
confirmation step and CSRF token keep a cross-site page from driving a signed-in browser into
a local listener. It exists because the retired React site used to provide that handoff. Set
`ANIMIND_SITE_URL` on the player to this API origin so it opens the backend page; a user can
still sign in directly inside the player instead. When Google sign-in is configured, the page
also offers **Continue with Google**, so a Google-only account can pair without a password.

## Security model

| Control | Implementation |
| --- | --- |
| Authentication | Administrator session resolved through the existing auth service on every request. |
| Authorization | `requireAdminPage` rejects a valid non-admin session with 403. |
| CSRF | Double-submit token: an HttpOnly cookie and a hidden form field, compared in constant time. `__Host-` prefix in Secure deployments. |
| Content security | `default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`; no inline scripts or styles. |
| Clickjacking | `X-Frame-Options: DENY` and `frame-ancestors 'none'`. |
| Rate limits | The shared `Login` bucket on the login routes and the `Admin` bucket on the console pages. |
| Session lifetime | Same sliding web-session lifetime as the rest of the API; logout revokes the row and clears the cookie. |

The panel is not a separate trust boundary from the API: an administrator can already call
`/api/admin/*`. The console exists to make those operations usable without curl.

## Deployment

- nginx already proxies `/` to the API, so `/admin` and `/admin/static/admin.css` work with
  the existing `deploy/nginx/nginx.conf`.
- Terminate TLS in front of nginx and keep `SECURE_COOKIES=true` in production. The CSRF
  cookie then uses the `__Host-` prefix.
- `PUBLIC_URL` should be the externally reachable API origin; `FRONTEND_URL` is still used
  for CORS and for the post-login redirect allowlist used by the native Google flow.
- No extra container or build step is needed.

## Legacy removal

The old website's `POST /api/rescan` endpoint and its camelCase `Anime` response have been
removed. The mobile app uses `POST /api/admin/scan` and the desktop player never called
either. The console uses the asynchronous `POST /admin/library/scan`.

If the old frontend is still deployed somewhere else, it is an independent deployment: retire
it. Nothing in this repository serves it.
