# Security

## Reporting a vulnerability

**Please do not open a public issue.**

Use GitHub's private vulnerability reporting on this repository: the "Report a
vulnerability" form under Security, or [github.com/FNXDOOM/Animind_Desktop_Backend/security/advisories/new](https://github.com/FNXDOOM/Animind_Desktop_Backend/security/advisories/new).
That creates a private advisory only you and the maintainers can see.

You should get a response within a few days. If the report is accepted, expect a fix and a
published advisory, with your name in it unless you would rather not be.

## What this software assumes about its operator

This is a personal streaming backend designed to run on a single VPS, operated by the same
person who uses it. Several decisions follow from that and are worth knowing before you
judge them as flaws.

It is **not** designed for:

- hosting unrelated third parties who must be isolated from each other,
- an untrusted network segment,
- a multi-tenant commercial service.

Running it for any of those is a different design, and the mitigations below are load-bearing
rather than belt-and-braces.

## The controls that matter

**Authentication is the only gate on playback, and it is stateless.** A stream URL carries
an HMAC-signed ticket scoped to one episode, one expiry, and optionally one audio track. It
contains no user identity, so it cannot browse the catalogue or read anyone's list. It is
still a bearer credential for that episode's bytes for its whole lifetime — do not paste
one somewhere public, and do not put request URLs containing `?st=` into a log that gets
shipped anywhere.

`DESKTOP_TOKEN_SIGNING_KEY` signs desktop tokens, bridge JWTs and playback tickets with the
same key. Rotating it invalidates every desktop session and every outstanding ticket at
once. That is the intended blast radius of a leaked key.

**Tokens are never stored in the clear.** Web sessions, desktop tokens and one-shot codes
are kept as SHA-256 hashes; a database dump does not yield working credentials. Passwords
use argon2id with deliberately expensive parameters.

**Ownership is structural, not validated.** No endpoint accepts a `user_id` from a client.
Every user-data query filters on the id taken from the authenticated session. A client
cannot ask for someone else's watchlist by naming them.

**Rate limiting runs before authentication**, so credential stuffing is bounded by a cheap
counter rather than by the password hash. Note that it keys on the **client IP**, which
means clients behind one NAT share a budget.

**`TRUST_PROXY_HEADERS` is false by default, and turning it on is a security decision.**
Accepting `X-Forwarded-For` from a proxy that appends rather than overwrites lets any client
choose its own rate-limit key. The bundled nginx config overwrites the header, which is why
the compose stack can safely set it to true.

**The metrics port is unauthenticated and must stay loopback-only.** `METRICS_ADDR`
defaults to `127.0.0.1:3002`, docker-compose does not publish it, and nginx returns 404 for
`/metrics`. Exposing it publicly discloses route traffic, library size and party counts.

**Watch-party sockets authenticate inside the Socket.IO CONNECT packet**, not in HTTP. The
global middleware chain still applies, but there is no session cookie or bearer header to
lean on at upgrade time; the token rides in the payload and is resolved by the same code the
REST API uses.

## Things that are true, so you can plan around them

- **Redis is not durable here, and does not need to be.** It holds resolved credentials for
  under a minute, rate-limit windows, and metadata documents. Losing it costs cache hits and
  one open window, never data. The compose service runs with persistence explicitly off.
- **Watch rooms live in process memory.** A restart ends every party. Rows in `watch_parties`
  are an audit trail, and startup marks any row still claiming to be active as ended.
- **`GET /api/hls/*` is unauthenticated by necessity.** A player fetches segments without
  headers. The control is that the session id is 16 random bytes, only a signed-in request
  can mint one, segment filenames are validated against a strict pattern and anything else is
  refused outright, and the segments live only in the server's own temp directory.
- **Subtitle sidecars and audio variants are written into your library directory**, next to
  the media. Treat that path as server-writable. In `s3` mode nothing is written at all.
- **The scanner prunes rows for files it did not see.** A dropped mount during a scan removes
  catalogue rows for that disk. `SCANNER_PRUNE_DELETED=false` disables the pruning.
- **A password-reset token is written to the server log.** There is no email transport in
  this project. Anyone with log access can reset any account. Do not ship logs to a system
  you have not considered that consequence for, and set up real delivery before letting
  people you do not know sign up.
- **The first administrator has no bootstrap path.** `is_admin` defaults to false and the
  only route that grants it requires admin, so the initial grant is a manual database
  update. That is deliberate obscurity, not a hole — but it does mean the very first account
  is created by whoever can reach Postgres.

## Hardening checklist for anything beyond a home network

1. TLS in front of nginx, and `SECURE_COOKIES` left on.
2. Keep `METRICS_ADDR` on loopback. Do not add a proxy rule for `/metrics`.
3. Set a real `WEBHOOK_SECRET`; the shipped default is a placeholder.
4. Generate `DESKTOP_TOKEN_SIGNING_KEY` randomly and keep it out of the repository.
5. Set `ALLOW_SIGNUP=false` once the accounts you want exist.
6. Configure `FRONTEND_URL` to exactly the origins you serve. An unset allowlist is not a
   restriction.
7. Put Postgres and Redis on loopback, which the bundled compose file already does.
8. Run the API as a non-root user, as the Dockerfile does.
9. Prefer `STORAGE_MODE=s3` if you do not want the server writing into your media tree.
10. Re-read the log-handling note above before exposing this to anyone else.

## Scope

This document describes the design as it is. It is not an assurance of absence of
vulnerabilities, and it is not legal advice. If you plan to run this for other people, a
review against your actual deployment is a reasonable thing to commission first.
