# Changelog

Notable changes to AniMind Backend (Go). The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**There are no tagged releases yet.** Everything below is unreleased work on `main`.
The sections exist so that changes land in the right place from now on rather than being
reconstructed from 60 commits afterwards.

## [Unreleased]

### Added

- **Torrent search.** Torznab indexers, a search pipeline with ranking, opaque result ids,
  a bounded client and a trending feed.
- **Downloads.** A job manager with per-user and global quotas, a state machine, resume on
  startup, file selection, an importer that writes selected files into the library, and a
  per-user Server-Sent-Events hub with snapshot and coalescing. Route surface:
  `POST/GET/PATCH/DELETE /api/downloads`, `GET /api/downloads/{id}`, `/api/downloads/events`.
- `migrations/0003_downloads.sql`, the download-job repository, and a Prometheus row plus a
  live verification script for the whole subsystem.
- Shared auth cache and observability; AniList response caching.
- A browser-facing Google entry point and app-link return targets.
- Password accounts can change their email address.
- **Admin console.** A server-rendered operator UI at `/admin` with overview stats, account
  search, administrator/download toggles, live app/browser session and desktop-device
  revocation, account deletion, library scans, audio-cache cleanup and show deletion. The
  templates and stylesheet are embedded in the binary; there is no separate frontend build.
- `/desktop-auth`, replacing the old website's pairing page: the Qt player opens it in a
  browser, signs in, and receives the 60-second bridge token on its loopback listener.
- `scanner.Status()`, so the console can show the last scan result and whether a scan is
  running without blocking the request.
- `migrations/0004_desktop_session_families.sql`: refresh-token family id, the previous
  hash used for reuse detection, and a non-sliding 90-day absolute expiry.
- A defensive security audit under `security-audit/`, including architecture, threat model,
  findings, dependency/infrastructure reviews, remediation plan, regression tests and a
  remediation-status log.

### Changed

- Line endings are pinned to LF via `.gitattributes`, so a Windows checkout no longer makes
  every file look unformatted to `gofmt`.
- Watch-party group starts are scheduled rather than immediate, and the buffer gate was
  hardened; drift corrections and private packets are now stamped.
- The watch-party snapshot publishes the parent show, not only the episode.
- Proxy and production defaults were locked down.
- Room codes are 7 characters instead of 5, and a per-account failed-join window slows
  guessing. Clients must treat the code as an opaque string.
- `/api/auth/me` uses the user-data rate-limit bucket instead of the login bucket, so a
  normal SPA session no longer hits 429 after 20 page loads.
- The runtime image is Alpine 3.22, digest-pinned; GitHub Actions are pinned to commit
  SHAs, and CI runs `govulncheck`.
- `gorilla/websocket` and the OpenTelemetry modules are upgraded to their fixed versions.
- A Secure deployment now sets the session cookie as `__Host-animind_session` and does not
  accept the legacy host-only name; the legacy cookie is cleared on logout. Existing browsers
  may need to sign in once after upgrading.
- The website is now the admin console only: `/` redirects to `/admin`, and the default
  `FRONTEND_URL` is the API origin rather than the retired Vite dev server.
- `User` carries `can_download`, so the account page can render the entitlement without a
  second projection.

### Removed

- The old website's `POST /api/rescan` endpoint, its camelCase `Anime` response, the
  `ShowSummary` projection and the dedicated rescan rate limiter. The mobile app uses
  `POST /api/admin/scan`; the console uses the asynchronous `POST /admin/library/scan`.
- The old-site-specific Google/desktop pairing page is no longer required: the backend serves
  `/desktop-auth` itself.

### Fixed

- `go test -race` failed in `internal/downloads`: eight assertions read the fake engine's
  call-count maps without taking the mutex that the manager's own goroutine writes them
  under. Every reported race was on a test double's own map, not on engine state. The whole
  repository is now race-clean, and CI runs it that way.
- Torrent clients can pin their listen address; port mapping is disabled.
- Torrents at terminal states stop, and seeding knobs are clamped.
- SSE subscribers close on shutdown.
- Imports are contained by symlink and root, and resume idempotently.
- Unselected packs resume into selection, and job ownership is enforced on every path.
- Nil-service wiring, admin patch semantics and typed duplicate errors in the download
  manager.
- The scanner no longer fabricates subtitle tags, and batch range separators are trimmed
  from delegated titles.
- OAuth and login hardening; polling events are authenticated.

### Security

- Password change, password reset and email change now revoke **desktop** sessions as well
  as web sessions. Previously a stolen web session could be exchanged for a 30-day desktop
  refresh token that survived the victim's remediation.
- Desktop refresh tokens rotate with reuse detection: replaying a rotated-out token revokes
  the whole token family. Rotation no longer extends the 90-day absolute lifetime.
- Bridge handoff tokens carry the browser-session row id and are rejected at
  `/api/auth/desktop/exchange` once that session is logged out.
- Google sign-in no longer silently attaches a verified Google identity to an existing
  **password** account. A pre-registered address cannot inherit the owner's Google login;
  the owner must use the password path.
- The login lockout response is now produced after the same number of failures for an
  unknown address as for a registered one, removing the account-existence oracle. Signup
  still answers 409 for a taken address until email verification exists.
- Socket.IO handshakes share a per-IP limiter, the hub caps peers at 1000, and an
  unauthenticated WebSocket or polling session is closed after 10 seconds without CONNECT.
- The public AniList proxy caches only documents up to 256 KiB; the in-process cache now
  enforces a 64 MiB byte budget in addition to its 50 000-entry cap.
- HLS seek, heartbeat and destroy are bound to the session owner; heartbeat no longer
  lists every active session.
- A wildcard CORS origin returns `Access-Control-Allow-Origin: *` without credentials; the
  exact-allowlist path keeps credentialed CORS.
- `ENV` is required when `PUBLIC_URL` is not loopback, production rejects the documented
  placeholder signing/webhook secrets, and `DEBUG_ERRORS` defaults to false so error
  details are opt-in.
- The recovery log no longer contains the email address or the reset-token hash. Session
  rows now record the client IP and user agent for the security screen.
- `ClientIP` parses IPv6 peers with `net.SplitHostPort` and validates forwarded addresses,
  so rate-limit and lockout keys are per address.
- The download quota count/insert decision is serialised in-process, and a unique-index
  race now maps to 409 instead of 500. The download entitlement lookup is a single-row
  query rather than a full user-table page scan.
- Dependency and build-input upgrades close the reachable `gorilla/websocket` and
  OpenTelemetry advisories; `govulncheck` reports no vulnerabilities.
- The admin console was reviewed again: a Secure deployment reads only the
  `__Host-animind_session` cookie (the legacy name is cleared but not accepted), account
  ids are UUID-validated before queries and redirects, both access flags are written
  atomically, and desktop counts honour the absolute expiry. See
  `security-audit/ADMIN_PANEL_REVIEW.md`.

### Follow-up / still to do

These items are deliberately not closed by the code commit; they need a product decision,
deployment action, or code outside this repository.

- **Password-reset delivery (F-14).** The API stores only the reset-token hash and cannot
  deliver the live token. Add an SMTP/provider integration before opening recovery to
  anyone other than the operator. Never log the plaintext token.
- **Email verification (F-04 residual).** Signup still accepts an unverified address. Add
  a verification state and require it before a password account can be linked to a
  provider identity. Then remove the 409 on signup if a uniform response is required.
- **Multi-replica download quota (F-16).** The quota lock is per process. A deployment
  with several API replicas needs a database advisory lock or a conditional insert.
- **Desktop pairing loopback port.** The stock player listens on fixed `127.0.0.1:27182`,
  so a malicious local process can bind it first and capture the bridge token. Change the
  player to choose a random port/nonce and pass it to `/desktop-auth`; the backend can then
  redirect only to that validated port. See `security-audit/ADMIN_PANEL_REVIEW.md`.
- **Remaining supply-chain items (F-18).** Pin the compose images by digest, add an
  SBOM/signature step, and update the development Prometheus/Grafana tags when the local
  stack is next refreshed.
- **Dead code (F-21).** `RegisterFailedLogin`, `LogoutEverywhere` and the unused
  `failed_login_count`/`locked_until` columns remain; either wire a real account-wide
  control or remove them.
- **Frontend stored-XSS review (P-01).** Subtitle text, watchlist `anime_data` and history
  `thumbnail_url` are returned as JSON; confirm the website and desktop clients render
  them without `innerHTML`/unsafe markup.
- **Torrent write-time paths (P-02).** The importer defends after download; add a
  write-time storage wrapper if the upstream torrent library's path handling is not
  trusted for hostile metainfo.
- **Media sandbox (P-03).** ffmpeg processes untrusted media; run it with no network, a
  read-only library and resource limits, and keep it patched.
- **Cloud/deployment controls (P-05, P-06).** Verify the S3 bucket is not public, keep
  `METRICS_ADDR` on loopback, terminate TLS in front of nginx, and replace every sample
  secret before exposing the stack.

### Documentation

- Comments that described behaviour the code does not have were corrected, including one
  claiming a packet-retransmit path that is not implemented and one attributing path-style
  addressing to a field nothing reads.

## Policy for this file

One line under `Unreleased` per user-visible change, added in the same pull request as the
code. Cosmetic internal refactors do not need an entry. When a version is tagged, promote
`Unreleased` to a dated section and start a new one.

[Unreleased]: https://github.com/FNXDOOM/Animind_Desktop_Backend/commits/main
