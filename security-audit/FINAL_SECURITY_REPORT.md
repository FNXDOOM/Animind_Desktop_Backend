# Final Security Report — Animind Backend (Go)

**Report ID:** ANIMIND-SEC-2026-11
**Classification:** Internal / owner
**Audit window:** 2026-11-25
**Snapshot:** `bf58946` plus uncommitted working-tree changes (see `AUDIT_CHECKLIST.md` §2)
**Scope:** the local repository and its deployment files. No external host, production system,
third-party service, or real user data was accessed.
**Method:** defensive source review, configuration review, dependency analysis, and proposed
local regression tests. No exploitation was performed.

> **Remediation update (2026-11-25):** the code fixes have been applied and verified; see
> `REMEDIATION_STATUS.md`. This report remains the pre-fix assessment.

---

## 1. Verdict

| Question | Answer |
|---|---|
| Critical issues? | **None found** |
| High issues? | **2** (F-01 persistent desktop session after password reset; F-02 unauthenticated socket exhaustion) |
| Medium issues? | **3** (F-03 cache memory exhaustion; F-04 pre-account takeover; F-05 insecure production defaults) |
| Low issues? | **11** |
| Informational | **6** |
| Verified strong controls | **20** |
| Most urgent action | Revoke desktop sessions on password change/reset/email change (F-01) |
| Second action | Bound socket handshakes and the AniList cache (F-02, F-03) |

The code is **not** in a "one missing check away from compromise" state. The core primitives
(authentication, password hashing, token handling, SQL scoping, path containment, command
construction, OAuth state) are implemented with care and are covered by meaningful tests. The
weaknesses are concentrated where the code crosses a lifecycle boundary (web vs desktop sessions),
where an unauthenticated feature meets an unbounded resource (sockets, cache), or where a
developer convenience default survives into a public deployment.

---

## 2. Scope and coverage

### Reviewed

- 107 Go files, ≈31 000 lines: every package under `cmd/` and `internal/`.
- Every route in `internal/httpx/server.go` and its middleware chain.
- Every SQL statement in `internal/repository/` and all three migrations.
- Authentication, OAuth, tokens, sessions, password reset, desktop pairing.
- Media streaming, range handling, HLS, subtitles, audio variants, ffmpeg invocation.
- Torrent search, download state machine, file selection, import, deletion.
- Watch-party hub, protocol, rooms, scheduling, persistence.
- Cache, rate limiting, metrics, scheduler, scanner, metadata clients.
- `Dockerfile`, `.dockerignore`, `docker-compose.yml`, nginx, Prometheus, Grafana, CI,
  Dependabot, `.env.example`, `.gitignore`, `SECURITY.md`.
- Git history filenames and a secret-pattern scan of the tracked tree.

### Not available / not assessable

- Website/SPA, desktop Qt player, mobile clients (frontend XSS sinks, client-side storage).
- TLS terminator, WAF/CDN, host firewall, cloud IAM/bucket policy, database role grants.
- Live runtime behaviour (no service was started for this audit); findings are code-path proofs.
- Semgrep, gitleaks, Trivy, Checkov, npm/pip audits (not installed); `gosec` was attempted but
  interrupted. `go vet`, `go test`, and `govulncheck` ran successfully.

These limitations are repeated in `AUDIT_CHECKLIST.md` §7 so the report is not read as a
guarantee of absence.

---

## 3. Findings summary

| ID | Title | Severity | Status | Main location |
|---|---|---|---|---|
| F-01 | Desktop credentials survive password reset/change/email change | High | Confirmed | `internal/auth/service.go` |
| F-02 | Unauthenticated Socket.IO connections unlimited/unthrottled | High (A) | Confirmed | `internal/httpx/server.go`, `internal/syncplay` |
| F-03 | Public AniList proxy cache can exhaust memory | Medium | Confirmed | `internal/httpx/handler_library.go`, `internal/cache` |
| F-04 | Unverified signup + Google email linking → pre-account takeover | Medium | Confirmed (conditional) | `internal/auth/service.go` |
| F-05 | Production hardening bypassed when `ENV` unset; placeholder secrets accepted | Medium | Confirmed | `internal/config/config.go`, deploy files |
| F-06 | User enumeration via lockout (423) and signup (409) | Low | Confirmed | `internal/auth/service.go`, `handler_auth.go` |
| F-07 | `ClientIP` mishandles IPv6; breaks limits and login storage | Low | Confirmed | `internal/httpx/responses.go` |
| F-08 | HLS seek/destroy/heartbeat not bound to session owner | Low | Confirmed | `internal/httpx/handler_media.go`, `internal/media/hls.go` |
| F-09 | Watch-party room codes brute-forceable without join throttling | Low | Confirmed | `internal/syncplay` |
| F-10 | Bridge token exchangeable for 60 s after logout | Low | Confirmed | `internal/auth/service.go` |
| F-11 | Refresh rotation lacks reuse detection; slides indefinitely | Low | Confirmed | `internal/auth/service.go` |
| F-12 | Wildcard CORS reflects origin with credentials outside production | Low | Confirmed | `internal/httpx/middleware.go` |
| F-13 | Reachable dependency vulnerabilities (gorilla/websocket, otel) | Low | Confirmed | `go.mod` |
| F-14 | Password reset cannot deliver a token | Info | Confirmed | `internal/httpx/handler_auth.go` |
| F-15 | Session cookie lacks `__Host-` prefix | Info/Low | Confirmed | `internal/httpx/server.go` |
| F-16 | Download quota TOCTOU | Low | Confirmed | `internal/downloads/manager.go` |
| F-17 | `can_download` lookup pages the user table | Info | Confirmed | `internal/httpx/handler_downloads.go` |
| F-18 | CI/image supply-chain hardening gaps | Low | Confirmed | `.github/workflows/ci.yml`, Docker/Compose |
| F-19 | Non-production error details leak internals | Info | Confirmed | `internal/httpx/handler_auth.go` |
| F-20 | Recovery flow logs the email address | Info | Confirmed | `internal/httpx/handler_auth.go` |
| F-21 | Dead lockout/session code misleads reviewers | Info | Confirmed | `internal/repository`, `internal/syncplay` |
| F-22 | Web sessions never record client IP or user agent | Low | Confirmed | `internal/auth/service.go`, `internal/httpx/handler_auth.go` |

Full evidence, preconditions, impact and patches: `SECURITY_FINDINGS.md`.

---

## 4. Verified controls (no finding)

These were actively checked; a regression here would be a new finding.

| # | Control | Evidence |
|---|---|---|
| V-01 | SQL injection prevented | Every repository call uses pgx parameters; `fmt.Sprintf` only builds placeholder indices/constant clause text |
| V-02 | Path traversal prevented for library reads | `storage.LocalPath`: lexical containment + symlink re-resolution; used by streaming, subtitles, HLS source |
| V-03 | HLS segment traversal prevented | `^seg\d{5}\.ts$` and `^[a-f0-9]{32}$` patterns, fail closed |
| V-04 | Torrent import escape prevented | `validateTorrentPath`, `escapesTorrentRoot`, `ensureSourceInsideJobDir`, `containedInLibrary` |
| V-05 | Command injection prevented | `exec.Command` argument vectors; no shell; ffmpeg indexes are integers; file paths from DB/`LocalPath` |
| V-06 | SSRF prevented | Every outbound URL is a configured endpoint (AniList, MAL, Google, OpenRouter, Torznab); no request-controlled URL reaches an HTTP client |
| V-07 | JWT forgery prevented | `alg` must be `HS256`; signature verified before payload decode; `aud`, `iss`, `sub`, `exp` enforced; `hmac.Equal` |
| V-08 | Password storage sound | argon2id, 16-byte random salt, constant-time compare, rehash on parameter change, dummy hash for unknown emails |
| V-09 | Token storage sound | Web, refresh, access and one-time codes stored as SHA-256; one-time tokens consumed atomically by `DELETE … RETURNING` |
| V-10 | OAuth flow sound | signed state, single-use cache consume, browser nonce, 10-min expiry, verified Google email, origin-allowlisted return target |
| V-11 | CSRF mitigated | `SameSite=Lax`, JSON bodies, state-changing methods only, no state-changing GET; OAuth state |
| V-12 | Webhook authenticated | constant-time shared-secret compare; production rejects the dev default |
| V-13 | Object-level authorization | SQL scoped by session user; download ownership re-checked; foreign ids return 404 |
| V-14 | Function-level authorization | every admin route has `RequireAuth` + `RequireAdmin`; self-demote guard |
| V-15 | Rate limiting before auth | per-route buckets, 18 presets, Redis-shared when configured; independent buckets prevent cross-feature starvation |
| V-16 | WebSocket origin check | `CheckOrigin` allowlists browser origins; desktop clients without Origin are accepted by design; pre-auth application events ignored |
| V-17 | Security headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` |
| V-18 | Non-root container | `USER animind`; no build toolchain in runtime image |
| V-19 | No tracked secrets | only `.env.example` with placeholders; `.env`/keys ignored; `go mod verify` passes |
| V-20 | Database integrity | UUID PKs, FKs with cascade, unique constraints on email/session hashes/active downloads, state CHECK |

---

## 5. Severity rationale and false-positive discipline

Each confirmed finding was run through the 7-Question Gate and the Pre-Severity Gate:

- F-01 is not "theoretical session theft": the code path that fails to revoke is present, and the
  attacker-side path (`desktop/exchange` from a cookie → 30-day refresh) is a normal feature. The
  High rating is conditional on prior credential theft and says so.
- F-02/F-03 are availability findings. They are not inflated to "Critical" because they do not
  read or modify data; their CVSS reflects availability only.
- F-04 is Medium, not High, because it requires the attacker to register before the victim and
  the victim to use Google sign-in. It is not a universal ATO.
- F-06 is Low, not Medium, because enumeration alone does not grant access.
- F-08/F-09/F-10/F-11/F-12 are Low because they require a leaked 128-bit id, a guessed live room
  code, a leaked bridge token, a stolen refresh token, or a wildcard configuration combined with
  a relaxed cookie policy, respectively.
- F-14 is Informational; it is an operational safety issue, and it would become High only if a
  plaintext token were logged (which the current code does not do).

Rejected signals (path parameters, unauthenticated HLS, `?st=` tickets, JWT header, CORS wildcard
alone, duplicate usernames, etc.) are recorded in `SECURITY_FINDINGS.md` §8 with the reason. No
finding in this report depends on a single ambiguous response or an unverified tool artefact.

---

## 6. Business impact

For the intended single-operator deployment:

- **Confidentiality:** no cross-user data leak found in the API. The realistic confidentiality
  risk is a stolen session/desktop token (F-01/F-10/F-11) and PII in logs (F-20). The S3 bucket
  policy is outside this review (P-05).
- **Integrity:** the same stolen-token paths allow modifying the victim's data and watch-party
  state. F-04 allows an attacker to seed an account the victim later uses. No SQL/command
  injection was found.
- **Availability:** F-02 and F-03 are the meaningful denial-of-service paths; ffmpeg/torrent
  processing remains an inherent resource risk. The dev compose has no resource limits.
- **Compliance/privacy:** the app stores email, IP, user agent and viewing history. There is no
  retention policy, no export/delete beyond account deletion (which cascades), and no
  email-verification evidence. For a personal instance this is acceptable; for third-party users
  it would need a privacy review (the SECURITY.md explicitly says the design is not for that).

---

## 7. Remediation order

| Order | Item | Why now |
|---|---|---|
| 1 | F-01 desktop revocation | Restores the meaning of password reset; one method + three call sites |
| 2 | F-02 socket bounds + nginx limits | Cheapest unauthenticated availability kill-switch to close |
| 3 | F-03 cache byte bound | Stops an unauthenticated OOM on the default deployment |
| 4 | F-05 fail-closed config | Prevents the most likely real-world compromise: a copied sample deployment |
| 5 | F-04 email verification / link proof | Product decision; needed before signup is opened to anyone untrusted |
| 6 | F-06/F-07/F-08/F-09/F-10/F-11/F-12 | Batch into one hardening release with tests |
| 7 | F-13/F-15/F-16/F-17/F-18/F-19/F-20/F-21 | Hygiene and supply chain |

Details, patches and acceptance criteria: `REMEDIATION_PLAN.md`. Proposed tests:
`REGRESSION_TESTS.md`.

---

## 8. Verification status

| Stage | Status |
|---|---|
| Source review | Complete for all in-repo components |
| Configuration review | Complete for Docker, compose, nginx, Prometheus, Grafana, CI, env template |
| Dependency review | Complete for Go; `govulncheck` run; helper scripts are stdlib-only |
| Secret scan | Complete for tracked files and history filenames; no tracked secret found |
| Dynamic testing | **Local only** — full suite run against a disposable PostgreSQL 16 container; no live/production service was touched |
| Patches applied | **2026-11-25** — see `REMEDIATION_STATUS.md` |
| Regression tests applied | **Yes** — new unit and integration tests listed in `REMEDIATION_STATUS.md` |
| Post-remediation verification | `go vet` clean; `go test -race -p 1 ./...` pass; Postgres-backed suite pass; `govulncheck` reports no vulnerabilities |
| Still open | F-14 (mail delivery), F-18 compose images/SBOM, F-21 dead repository methods, F-16 multi-replica quota, P-01…P-06 deployment/frontend items |

A finding is not "resolved" until the corresponding test fails before the patch and passes after
it, and the full suite (`go test -race -p 1 ./...`) remains green.

---

## 9. Residual risk statement

Even after every recommendation is applied, the following remain true by design or by
deployment:

1. A bearer token is a bearer credential; TLS at the edge is mandatory, and the repository's
   nginx does not provide it.
2. Any user with `can_download` can introduce untrusted media into the library; ffmpeg and the
   torrent parser are the highest-risk code paths and should be sandboxed and patched.
3. The single-operator model has no tenant isolation; `can_download` and admin are the only
   privilege tiers.
4. The S3 bucket policy, database grants, host firewall and backup integrity are deployment
   controls not visible in this repository.
5. Password reset is not deliverable; until an integration exists, account recovery is an
   operator procedure.
6. The absence of MFA means a phished or reused password is sufficient for account access; the
   per-IP limiter does not stop a distributed attack.

These are stated so the owner can decide which are acceptable for their deployment rather than
discovering them during an incident.

---

## 10. Sign-off

This report reflects the repository state on 2026-11-25 and the evidence recorded in
`SECURITY_FINDINGS.md`. It is a point-in-time review, not a guarantee. The next review should
occur after the P0/P1 patches land, after any change to the auth/session model, and before the
service is exposed to users outside the operator's household.
