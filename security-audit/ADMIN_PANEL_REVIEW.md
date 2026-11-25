# Admin Console Review — 2026-11-25

Second-pass defensive review of the admin console and desktop pairing work, plus a regression
pass over the authentication/session paths. This is the review requested after the first
audit; the original findings remain in `SECURITY_FINDINGS.md`.

**Verdict:** no high-confidence remotely exploitable issue remains in the new code. Six issues
were found during this pass and fixed (four security/hardening items, two code-quality items);
the remaining risks are deployment or client constraints and are listed under *Residual
risks*.

## Skills and tooling

Skills invoked: `security-review`, `hunt-auth-bypass`, `hunt-csrf`, `hunt-idor`, `hunt-xss`,
`hunt-session`, `hunt-open-redirect`, `hunt-brute-force`, `hunt-api-misconfig`,
`triage-validation`.

Commands run:

```bash
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go test -count=1 -p 1 ./...            # against disposable PostgreSQL 16
go test -count=1 -p 1 -race ./...
```

The full suite includes the new admin tests (`TestAdminDashboardLoadsForAdmin`,
`TestAdminUsersPageAndDetail`, `TestAdminFormsRequireCSRF`, `TestAdminSessionRevocation`,
`TestDesktopAuthPairsAnyAccount`, `TestSecureDeploymentIgnoresLegacySessionCookie`).

## Findings

| ID | Severity | Status | Finding |
|---|---|---|---|
| A-01 | Medium (conditional) | Fixed | A Secure deployment still accepted the legacy `animind_session` cookie, weakening the `__Host-` cookie-tossing boundary |
| A-02 | Low | Fixed | Admin action target ids were used in redirects without UUID validation; malformed input could produce a database error and a raw `Location` |
| A-03 | Low | Fixed | Administrator/download entitlement changes were two statements and could land half-applied |
| A-04 | Low | Fixed | Desktop session lists and counts ignored the 90-day absolute expiry |
| A-05 | Info | Fixed | Scanner contained an unused grouping slice whose comment claimed metadata was resolved once per show; the optimization was never wired |
| A-06 | Info | Fixed | `runLibraryScan` could panic on nil scanner/storage in an unusual embedding |

### A-01 — Legacy session cookie accepted in Secure deployments

**Where:** `internal/httpx/server.go:credential`.

The previous helper read `__Host-animind_session` first and then the legacy host-only
`animind_session`. The fallback was left for a rollout, but it meant a Secure deployment
still trusted a cookie a sibling subdomain can set with `Domain=.example.com`. The
`__Host-` prefix exists precisely to prevent that, so the guarantee was incomplete.

**Fix:** `credential` is now a `*Server` method and reads only the cookie name for the active
deployment: `__Host-animind_session` when `SECURE_COOKIES=true`, `animind_session` otherwise.
`clearSessionCookie` still expires both names so an old cookie is removed. Existing browsers
may need one sign-in after upgrading.

**Verification:** `TestSecureDeploymentIgnoresLegacySessionCookie` proves the legacy cookie is
ignored in Secure mode and still accepted in development.

### A-02 — Admin target ids were not validated before redirects

**Where:** `internal/httpx/adminui.go` account actions.

A malformed `{id}` could reach a `::uuid` query (database error) and was concatenated into a
redirect target. The actions are admin-only, but the handler should not turn bad input into a
500 or an unchecked `Location`. `credential` plus `adminCSRF` already gate the route; this is
defense in depth.

**Fix:** `adminUserParam` checks `repository.IsUUID` and redirects to the account list with a
fixed error code. `redirectAdmin` additionally accepts only `/admin` and `/desktop-auth` paths,
rejects absolute URLs and `..`/`//` normalization, and carries a documented `#nosec G710`
because the redirect is now explicitly same-origin.

### A-03 — Access flags updated in two statements

**Where:** `internal/httpx/adminui.go:handleAdminUIUserFlags`.

`SetAdmin` followed by `SetCanDownload` could leave the account promoted but the download
entitlement unchanged if the second statement failed.

**Fix:** `repository.SetUserAccess` writes both columns in one `UPDATE`; the handler maps
`ErrNotFound` to `user_not_found` instead of a generic failure.

### A-04 — Desktop session counts ignored absolute expiry

**Where:** `internal/repository/desktopsessions.go:ListDesktopSessions`,
`internal/repository/admin.go:SessionCounts` and `ListUsersWithSessions`.

The refresh-token expiry was checked, but the 90-day `absolute_expires_at` was not, so the
admin and security screens could list a device the refresh endpoint would already reject.

**Fix:** all three live-session queries now require
`refresh_token_expires_at > now() AND absolute_expires_at > now()`.

### A-05 — Unused scanner grouping

**Where:** `internal/scanner/scanner.go:Run`.

The loop built a `groups` slice and per-group `files`, then never read them. The comment
claimed a 200-file show resolved metadata once, but `indexFile` still called `resolveShow` per
file. This was dead code with a misleading comment, not a security defect.

**Fix:** removed the unused group slice and corrected the comment. The show row is still
resolved per file; the metadata service caches upstream lookups. If scan time on very large
shows becomes a problem, resolving once per normalized title is the follow-up optimization.

### A-06 — Nil scanner/storage panic path

**Where:** `internal/httpx/handler_library.go:runLibraryScan`.

The helper assumed both collaborators were wired. The admin UI checks the scanner, but the
JSON `/api/admin/scan` path did not. Real `cmd/server` wiring always provides them; an
embedding could panic.

**Fix:** `runLibraryScan` returns a clear error when the scanner is nil, and the prewarm branch
also requires `s.storage != nil`. The handler maps the error to its normal 500 response.

## Reviewed controls (no finding)

| Control | Result |
|---|---|
| Admin authentication | Every `/admin` page and action is inside `requireAdminPage`; `/admin/static` and `/admin/login` are the only public admin paths. |
| Admin authorization | A valid non-admin session gets 403; a desktop admin bearer is still admin-authorized. |
| CSRF | All state-changing forms use the double-submit token, `SameSite=Strict`, `HttpOnly`, and `__Host-` in Secure mode. `text/plain` and JSON bodies do not populate `PostFormValue`, so they fail closed. |
| Session fixation | Login creates a fresh session; no pre-auth session is adopted; Secure mode reads only the `__Host-` cookie. |
| Logout/password/email change | Logout revokes the row; password/reset/email changes revoke web and desktop sessions (first-pass fixes). |
| IDOR | Admin actions intentionally address any account; non-admin callers cannot reach them. `/desktop-auth` only uses the caller's own session. |
| XSS | Templates use `html/template` with contextual escaping; no `template.HTML`, no inline scripts or styles. User data (email, username, device name, show title/studio, search, errors) is escaped. CSP has no script source. |
| Open redirect | Admin redirects are same-origin checked; `/desktop-auth` targets the fixed loopback address; Google returns go through `sanitizeReturnTarget` and the signed state. |
| Rate limits | Login 20/min, admin pages 60/min, scan has a 5s launch cooldown and the scanner serializes runs. |
| SQL injection | All admin queries use placeholders; search uses `escapeLike`; no string interpolation of user data. |
| Request size | Admin forms and desktop-auth forms are capped at 1 MiB. |
| Dependency surface | No new modules; `govulncheck` reports no vulnerabilities. |
| Headers | CSP, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on the console. |

## Residual risks

1. **Fixed loopback pairing port (Medium, local attacker).** `/desktop-auth` redirects the
   bridge token to `127.0.0.1:27182`, the port hard-coded in the stock Qt player. A malicious
   local process can bind that port before the player and capture the 60-second bridge token,
   then exchange it for a desktop refresh token. The page now requires an explicit
   confirmation and sends `Referrer-Policy: no-referrer`, but the port is still predictable.
   Fixing this properly needs a client change: the player should choose a random port/nonce
   and pass it to the page, and the server should redirect only to that validated port.
2. **Admin login is email/password only.** The console deliberately does not offer Google, so
   an admin account needs a password. Promote a password account; a Google-only account
   cannot open the console.
3. **Old website deployment.** Nothing in this repository serves it, but an external
   deployment must still be retired. The old `/api/rescan` route is gone (404).
4. **Desktop `ANIMIND_SITE_URL`.** The stock player defaults to the old site origin. Set it
   to the API origin so the pairing button opens `/desktop-auth`; in-app password sign-in
   still works.
5. **Single-process state.** Scanner status and rate limits are per process. A multi-replica
   deployment needs shared state for both.
6. **Audit trail.** Admin actions are logged with admin/target ids, but there is no
   append-only audit table. The process log is the record.

## Tool notes

- `gosec` flags G118 (background scan context), G124 (dynamic `Secure` on the CSRF cookie),
  and G710 (redirect helper). The first two are intentional and documented at the call site;
  G710 is now same-origin validated and suppressed with a reason.
- Pre-existing `gosec` findings remain outside this change: media/cache directory modes
  (G301/G306), SHA-1 used for cache keys (G401/G505), ffmpeg argument construction (G204),
  and path-controlled file reads (G304). These were reviewed in the first audit and are not
  new; the deployment notes still apply.
- `staticcheck` reports only style/unused items outside the new code after this pass; the
  new admin and desktop-auth files are clean.

## Verification

```
go vet ./...                                      clean
staticcheck ./internal/httpx ./internal/scanner   no new-code findings
gosec ./internal/httpx                           new-code findings reviewed/suppressed
go test -count=1 -p 1 ./...                      pass (PostgreSQL 16, migration 0004)
go test -count=1 -p 1 -race ./...                pass
govulncheck ./...                                No vulnerabilities found
```

The live panel was also exercised against a disposable database: login, overview, account
list, account detail, session/device revoke, CSRF rejection, and a real library scan with
status/result rendering.
