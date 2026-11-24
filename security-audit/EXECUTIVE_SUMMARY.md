# Executive Summary — Animind Backend (Go) Security Audit

**Audit type:** Defensive source-code and configuration audit (local repository only)
**Target:** `Animind_Backend_Go`
**Snapshot:** commit `bf58946` plus the uncommitted working-tree changes listed in `AUDIT_CHECKLIST.md`
**Date:** 2026-11-25
**Auditor:** AI-assisted review using the installed Claude-BugHunter skill set
**Authorisation:** Owner-requested review of their own local repository, synthetic data only. No external host, production system, or third-party service was attacked.

> **Remediation update (2026-11-25):** the confirmed findings were fixed in the working tree and
> verified with `go vet`, `go test -race -p 1 ./...` (including a disposable PostgreSQL 16
> instance) and `govulncheck`. See `REMEDIATION_STATUS.md`. The counts and impact text below
> describe the **pre-fix** state.

---

## 1. Bottom line

The backend is **materially better secured than a typical application of this size**. The team has
already implemented the controls that most audits spend their first week asking for:

- argon2id password hashing with per-password salts, constant-time comparison and transparent rehash;
- opaque session tokens stored only as SHA-256 hashes;
- HS256 tokens with the algorithm pinned, audience/issuer/expiry enforced and constant-time HMAC comparison;
- server-side ownership scoping in SQL for every user-owned row;
- parameterised SQL everywhere (no string-built SQL values);
- a local-file resolver that refuses traversal and re-checks symlinks;
- constant-time webhook-secret comparison;
- signed, single-use, browser-bound OAuth state with `email_verified` enforcement;
- per-route rate limiting that runs before authentication;
- non-root container user, read-only CI permissions, and no tracked secrets.

No **Critical** issue was found. In particular, the audit did **not** find:

- SQL injection,
- command injection,
- path traversal to arbitrary files,
- SSRF to an attacker-chosen host,
- JWT algorithm confusion / `alg:none`,
- a missing-admin-check on an admin route,
- a cross-user IDOR in the user-data, download, session or party APIs,
- an unauthenticated RCE or an authentication bypass.

The real risk is concentrated in **session lifecycle**, **resource-exhaustion surfaces**, and
**deployment defaults** rather than in injection or access-control primitives.

### Severity counts

| Severity | Count | IDs |
|---|---|---|
| Critical | 0 | — |
| High | 2 | F-01, F-02 |
| Medium | 3 | F-03, F-04, F-05 |
| Low | 11 | F-06 … F-13, F-16, F-18, F-22 |
| Informational | 6 | F-14, F-15, F-17, F-19, F-20, F-21 |
| Verified controls (no finding) | 20 | see `FINAL_SECURITY_REPORT.md` §4 |

---

## 2. The three findings to fix first

### F-01 — Desktop credentials survive password reset / password change / email change (High)

`ChangePassword`, `ResetPassword` and `ChangeEmail` revoke **web** sessions only
(`RevokeAllWebSessions`). The `desktop_auth_sessions` rows are untouched, and a desktop refresh
token has a **30-day** lifetime that is renewed on every refresh. Because
`/api/auth/desktop/exchange` accepts a web session cookie, a stolen web session can be converted
into a desktop refresh token *before* the victim resets their password. The result: a user who
does the correct thing after a compromise (change the password) does **not** evict the attacker.

This is the highest-value fix in the repository: it is cheap (one repository method + three call
sites) and it restores the meaning of the account-recovery flow.

### F-02 — Unauthenticated socket connection exhaustion (High availability)

`/api/socket.io` is registered directly on the root chi router. It is **not** behind any of the
per-route limiters, has no concurrent-connection cap, and a polling `GET` creates a peer and a
`keepAlive` goroutine before any authentication happens. A client can hold websockets open
indefinitely by answering pings. This contradicts the statement in `SECURITY.md` that the global
middleware chain bounds the socket surface.

### F-03 — Unauthenticated AniList cache can exhaust memory (Medium)

`POST /api/anilist` is public and caches upstream responses for 12 hours. The response is read
with a 4 MiB cap, but the cache constrains **entry count** (50 000) and not **bytes**. On the
default no-Redis deployment, 50 000 × up to 4 MiB is a process-killing memory budget before
eviction ever runs.

---

## 3. What is already good and should not be "fixed"

Listing these explicitly prevents a later reviewer from regressing them:

- `internal/auth/jwt.go` rejects any `alg` other than `HS256`, checks the signature before
  decoding the payload, and requires a non-empty subject plus the expected audience/issuer.
- `internal/auth/service.go` absorbs unknown-email login timing with a dummy argon2 run.
- `internal/storage/storage.go:LocalPath` performs a lexical containment check and then resolves
  symlinks before allowing a read.
- `internal/media/hls.go` validates segment names with `^seg\d{5}\.ts$` and refuses anything else.
- `internal/downloads/files.go` rejects `..` escapes, absolute paths, Windows device names and
  overlong segments before a torrent file touches the library.
- `internal/httpx/handler_library.go` compares the storage webhook secret with
  `subtle.ConstantTimeCompare`.
- `internal/repository/userdata.go` scopes every statement by `user_id`; the handlers never accept
  a `user_id` from the client.
- `internal/httpx/server.go` marks the session cookie `HttpOnly`, `SameSite=Lax` and `Secure`
  when configured.

---

## 4. Deployment posture

The application code is ahead of the shipped deployment examples. `docker-compose.yml` is clearly
labelled local-development, but it publishes nginx on `0.0.0.0:8080`, sets `ENV=development`, and
ships known placeholder secrets and a default database password. A copy-paste deployment on a VPS
would therefore start with:

- no TLS (nginx listens on port 80 only),
- session cookies without `Secure`,
- a published, well-known `DESKTOP_TOKEN_SIGNING_KEY` and `WEBHOOK_SECRET`,
- Grafana `admin`/`admin` on loopback,
- no CPU/memory limits on the API or ffmpeg.

`ENV` also defaults to `development`, and the production-only validation only fires when the
literal value is `production`. This is finding F-05. The sample signing key
`replace-with-a-long-random-secret` is 33 characters and passes the "at least 32" check; the
production guard does not reject it.

Recommendation: make the safe path the default (secure cookies unless explicitly opted out,
reject known placeholder values in every environment, refuse to boot with `ENV` unset when a
public URL is configured), and make the unsafe path loud.

---

## 5. Coverage and limitations

- **Reviewed:** all 107 Go files (≈31 000 lines), all migrations, routing, middleware, auth,
  repository SQL, media/ffmpeg argument construction, torrent import, download state machine,
  syncplay hub, cache, configuration, Dockerfile, compose, nginx, Prometheus/Grafana deployment
  files, CI workflow, Dependabot configuration, and the tracked environment template.
- **Not in this repository:** the website, desktop player, mobile clients, and a TLS terminator.
  Frontend-rendered XSS sinks (subtitle content, `anime_data`, `thumbnail_url`) could therefore
  not be confirmed end-to-end and are recorded as conditional notes, not findings.
- **Tools:** `go vet` clean; `go test ./...` passes; `govulncheck` reports two reachable module
  vulnerabilities and one package-level vulnerability. `gosec` was attempted but interrupted;
  Semgrep, gitleaks, Trivy, Checkov and npm/pip audits were not installed in this environment.
  Those gaps are recorded in `DEPENDENCY_AUDIT.md` and `AUDIT_CHECKLIST.md`.
- **No exploitation** was performed against a running instance. Findings are proven by code path
  analysis and, where practical, by the proposed regression tests in `REGRESSION_TESTS.md`.
  F-02/F-03 are availability findings whose severity depends on the deployment's resource limits;
  that dependency is stated explicitly rather than assumed away.

---

## 6. Bottom line for the owner

Fix F-01 before exposing password reset to anyone else. Add socket and cache bounds (F-02, F-03)
before exposing the API to the public internet. Then work through F-04/F-05, which are
product/configuration decisions rather than code defects. Everything below that is hardening.

Full detail: `SECURITY_FINDINGS.md`. Prioritised work: `REMEDIATION_PLAN.md`. Verification
evidence and proposed tests: `REGRESSION_TESTS.md`.
