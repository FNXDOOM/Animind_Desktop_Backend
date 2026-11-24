# Audit Checklist & Coverage Log

**Purpose:** a single tracking file for every security category, its review status, the skills
applied, the findings raised, and the verification state. `SECURITY_FINDINGS.md` holds the
evidence; this file proves coverage and records the negatives.

> **Remediation update (2026-11-25):** the confirmed code findings were fixed and verified; see
> `REMEDIATION_STATUS.md`. The statuses below are the pre-fix review record.

---

## 1. Snapshot and working-tree state

| Item | Value |
|---|---|
| Base commit | `bf58946` — "fix(test): take the engine lock when reading call counts in downloads tests" |
| Branch | `main` |
| Working tree | 10 modified files, 2 untracked torrent DB files (see §2) |
| Go version | 1.27.4 |
| Files reviewed | 107 `.go` files, 3 migrations, 7 deployment/CI files, 1 env template |
| Audit date | 2026-11-25 |

The audit was performed on the **working tree**, including the uncommitted hardening changes.
Those changes are user work in progress and were not modified, reverted, staged or committed.

## 2. Uncommitted changes present at audit time

```
 M .env.example
 M SECURITY.md
 M docs/02-authentication.md
 M go.mod
 M go.sum
 M internal/auth/auth_test.go
 M internal/auth/google.go
 M internal/auth/service.go
 M internal/config/config.go
 M internal/config/config_test.go
?? .torrent.db-shm
?? .torrent.db-wal
```

They add: Go 1.27.4 and dependency upgrades, Google `email_verified` enforcement, a production
config guard (secure cookies + no wildcard origin), the corrected OAuth callback derivation, and
documentation clarifications. None of them conflicts with the findings; F-05 and F-01 remain true
after applying them.

## 3. Category checklist

Status legend: **Clean** = reviewed with no finding; **Finding** = one or more confirmed
findings; **Partial** = reviewed but a dependent component is absent; **N/A** = class absent.

### 3.1 Authentication and session management

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Password hashing/storage | Clean | `hunt-auth-bypass` | — | argon2id, salt, constant-time, rehash; tests exist |
| Password policy | Clean (weak by design) | `hunt-brute-force` | — | 8–256 chars; no breached-password/MFA (residual risk) |
| Password reset security | **Finding** | `hunt-forgot-password`, `hunt-ato` | F-01, F-14, F-20 | token 32 B, hash at rest, single-use, 1 h; delivery absent; desktop not revoked |
| Session expiration/invalidation | **Finding** | `hunt-session` | F-01, F-10, F-11, F-15 | web path good; desktop lifecycle gaps |
| Session metadata recording | **Finding** | `hunt-session`, `code-review` | F-22 | IP/user agent never attached on login/signup/OAuth |
| JWT validation | Clean | `hunt-jwt-crypto` | — | HS256 pinned, aud/iss/exp, HMAC constant-time |
| OAuth configuration | **Finding (product)** | `hunt-oauth` | F-04 | state/nonce/allowlist solid; email-linking trust gap |
| Refresh-token management | **Finding** | `hunt-session` | F-11 | rotation works; no family/reuse detection/absolute cap |
| Cookie attributes | **Finding (hardening)** | `hunt-session` | F-15 | HttpOnly/Lax host-only; no `__Host-`; Secure conditional |
| Account verification | **Finding (product)** | `hunt-ato` | F-04 | none; signup immediately authenticates |
| Authentication middleware coverage | Clean | `hunt-auth-bypass` | F-02 (socket pre-auth by design) | every REST route classified; socket auth is in CONNECT |
| Login rate limiting | **Finding (enumeration)** | `hunt-brute-force` | F-06, F-07 | per-IP + per-(email,source); lockout response differs |
| MFA | N/A | `hunt-mfa-bypass` | — | not implemented; not claimed |
| SAML/SSO | N/A | `hunt-saml`, `hunt-auth-bypass` | — | no SAML |
| CAPTCHA | N/A | `hunt-captcha-bypass` | — | no CAPTCHA |

### 3.2 Authorization and access control

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Broken object-level authorization | Clean | `hunt-idor` | F-08 (partial, HLS) | SQL scoping + ownership checks |
| Broken function-level authorization | Clean | `hunt-auth-bypass` | — | admin middleware on every admin route |
| Role-based access control | Clean | `hunt-idor` | — | `is_admin`, `can_download`; admin bypass documented |
| Ownership validation | Clean | `hunt-idor` | — | session user id only; body ids rejected on watchlist |
| Administrative access restriction | Clean | `hunt-auth-bypass` | — | `RequireAdmin`; self-demote guard |
| Tenant/user data isolation | Clean (single-operator) | `hunt-idor` | — | per-user SQL; no multi-tenant claim |
| Server-side authorization enforcement | Clean | `hunt-auth-bypass` | — | no client-only gate on a server route |
| HLS/watch-party object authz | **Finding** | `hunt-idor`, `hunt-websocket` | F-08, F-09 | session id/room code is the only key |

### 3.3 Input validation and injection

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| SQL injection | Clean | `hunt-sqli` | — | parameterised queries; `escapeLike` |
| NoSQL injection | N/A | `hunt-nosqli` | — | PostgreSQL only |
| XSS (reflected/stored/DOM) | Partial | `hunt-xss`, `hunt-dom` | P-01 | API JSON + `nosniff`; client rendering absent |
| Command execution | Clean | `hunt-rce` | — | no shell; argument vectors; integer indexes |
| SSTI | N/A | `hunt-ssti` | — | no template engine on request data |
| Path traversal | Clean | `hunt-lfi` | P-02 (download-time) | `LocalPath`, HLS regex, importer guards |
| XML parser security | Clean | `hunt-xxe` | — | Go `encoding/xml`, no external entities |
| Unsafe deserialization | Clean | `hunt-deserialization` | — | JSON only; no pickle/gob/Java/PHP/.NET |
| SSRF | Clean | `hunt-ssrf` | — | all outbound endpoints config-fixed |
| Parameter validation | Clean | `hunt-api-misconfig` | F-16 (business) | ranges, enums, `DisallowUnknownFields` |
| Output encoding | Clean (API) | `hunt-xss` | — | JSON content type, `nosniff` |
| Mass assignment | Clean | `hunt-api-misconfig` | — | `DisallowUnknownFields`; explicit DTOs |
| HTTP parameter pollution | Clean | `hunt-api-misconfig` | — | chi `URL.Query().Get` first value; no HPP-sensitive logic |
| Header injection | Clean | `hunt-host-header` | — | no user input in response headers except validated/numeric |
| Host header | Clean | `hunt-host-header` | — | issuer/redirects config-driven; no reset mailer |
| Open redirect | Clean | `hunt-open-redirect` | — | exact origin allowlist |
| Prototype pollution | N/A | `hunt-api-misconfig` | — | Go, not JS |

### 3.4 Browser and API security

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| CORS | **Finding (hardening)** | `hunt-cors` | F-12 | exact allowlist in prod; wildcard reflects origin in dev |
| CSRF | Clean | `hunt-csrf` | — | SameSite=Lax, JSON, non-GET; no CSRF token needed today |
| CSP | N/A | — | — | API serves no HTML; clients own CSP |
| Clickjacking | Clean | `hunt-clickjacking` | — | `X-Frame-Options: DENY` |
| Security headers | Clean | — | — | nosniff, DENY, no-referrer, no-store |
| API schema validation | Clean | `hunt-api-misconfig` | — | strict JSON decode; range checks |
| Excessive data exposure | Clean | `hunt-misc` | — | `PublicUser` excludes hashes; episodes hide path/bucket |
| Request size limits | Clean | — | — | 1 MiB JSON, 1 MiB headers, 8 MiB nginx, 4 MiB AniList response |
| WebSocket authorization | **Finding** | `hunt-websocket` | F-02, F-09 | CONNECT auth + participant check; resource limits absent |
| CSWSH | Clean | `hunt-websocket` | — | Origin allowlist; browser cannot omit Origin |
| Socket message tampering | Clean | `hunt-websocket` | — | participant check; host role not privileged |
| GraphQL security | Clean | `hunt-graphql` | F-03 (resource) | AniList proxy only; read-only query check |

### 3.5 File handling

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| File upload validation | N/A (no multipart endpoint) | `hunt-file-upload` | — | torrent import is the only file ingress |
| File size restrictions | Clean | `hunt-file-upload` | — | per-job size, `MaxFiles`, 4/8 MiB readers |
| MIME type validation | N/A | `hunt-file-upload` | — | media is served by extension; no upload |
| File storage permissions | Partial | — | — | non-root container; library mount is operator-controlled |
| Filename sanitisation | Clean | `hunt-lfi` | — | show title, subtitle language, HLS segment patterns |
| Secure file downloads | Clean | `hunt-idor` | F-08 (HLS control) | auth/ticket; range handling; `Content-Disposition` not used |
| Temporary file management | Clean | — | — | `os.CreateTemp` + rename; HLS dir removed on destroy |
| Zip/archive extraction | N/A | `hunt-file-upload` | — | none |

### 3.6 Cryptography

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Weak algorithms | Clean | `hunt-jwt-crypto` | — | SHA-256 HMAC, argon2id; SHA-1 only for cache filenames |
| Insecure RNG | Clean | `hunt-jwt-crypto` | — | `crypto/rand` for tokens/ids; `math/rand` absent from security paths |
| Hardcoded keys | Clean | `hunt-source-leak` | F-05 (placeholders) | no real key in tree; sample values rejected? (F-05) |
| Encryption configuration | N/A | — | — | no at-rest encryption claimed; tokens hashed; TLS at edge |
| Sensitive data encryption | Clean | — | — | passwords argon2id; tokens hashed |
| TLS configuration | **Finding (deployment)** | `hunt-tls-network` | F-05 | nginx HTTP-only; TLS assumed external |
| Key rotation | Partial | — | F-01/F-11 | signing-key rotation invalidates all; no per-session key |

### 3.7 Secrets and privacy

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Hardcoded credentials | Clean | `hunt-source-leak` | — | only test constants; no tracked `.env` |
| Exposed API keys | Clean | `hunt-source-leak` | — | none in tree/history scan |
| Sensitive env vars | **Finding (defaults)** | `hunt-tls-network` | F-05 | sample values + `ENV` default |
| Credentials in Git | Clean | `hunt-source-leak` | — | `git ls-files`/history filename scan clean |
| Sensitive data in logs | **Finding (low)** | `evidence-hygiene` | F-20 | recovery email logged; no tokens/passwords |
| PII exposure | Partial | `hunt-misc` | F-20 | email/IP/history stored; admin list includes email |
| DB connection security | Partial | — | — | `sslmode=disable` in dev compose; operator must use TLS remotely |
| Excessive data collection | Clean | — | — | only what the product needs |
| Error messages | **Finding (low)** | `hunt-exceptional-conditions` | F-19 | details leaked outside production |

### 3.8 Infrastructure and deployment

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Container permissions | Clean | `hunt-cloud-misconfig` | — | non-root |
| Root containers | Clean | — | — | `USER animind` |
| Excessive capabilities | **Finding (hardening)** | — | F-18 | no cap drop / no-new-privileges / read-only root |
| Public internal services | **Finding (defaults)** | `hunt-cloud-misconfig` | F-05 | nginx 0.0.0.0:8080; metrics loopback |
| Unsafe volume mounts | Partial | — | — | library is read-write for subtitles/audio; expected |
| Reverse proxy config | **Finding (hardening)** | `hunt-http-smuggling` | F-05/F-18 | no TLS/HSTS/limit_req; XFF overwrite is good |
| Nginx configuration | Partial | — | F-05 | server_tokens off; no TLS; logs query strings |
| Firewall | Not assessable | — | — | host control |
| Cloud IAM | Not assessable | `cloud-iam-deep` | P-05 | bucket policy outside repo |
| Database network exposure | Clean (compose) | — | — | loopback-only |
| Monitoring dashboard | **Finding (defaults)** | — | F-18 | Grafana admin/admin default |
| Production debug settings | **Finding** | — | F-05, F-19 | `ENV` default enables dev behaviour |
| Dev services in prod | **Finding** | — | F-05 | compose is dev-only but copyable |

### 3.9 Dependencies and supply chain

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Known vulnerable dependencies | **Finding** | `supply-chain-attack-recon` | F-13 | govulncheck: 2 reachable + 1 package |
| Unsupported versions | **Finding** | `hunt-cicd` | F-18 | Alpine 3.20 EOL window |
| Lockfile presence | Clean | — | — | `go.sum`; `go mod verify` pass |
| Unmaintained libraries | Clean-ish | — | — | active modules; two pseudo-versions |
| Insecure CI workflows | **Finding (hardening)** | `hunt-cicd` | F-18 | tag-pinned actions, no scans |
| Excessive pipeline permissions | Clean | `hunt-cicd` | — | `contents: read` |
| Unpinned images | **Finding** | `hunt-cicd` | F-18 | tags, not digests |
| Untrusted install scripts | Clean | — | — | no postinstall/curl-pipe |
| Integrity checks | Partial | — | — | Go checksums; no SBOM/signing |

### 3.10 Business logic and concurrency

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| State validation | Clean | `hunt-business-logic` | — | download CAS state machine |
| Invalid state transitions | Clean | — | — | `canTransition` |
| Duplicate transactions | **Finding (low)** | `hunt-race-condition` | F-16 | duplicate create races to 500 |
| Race conditions | **Finding (low)** | `hunt-race-condition` | F-16 | quota TOCTOU; HLS cap TOCTOU noted |
| Concurrency | Clean | `hunt-race-condition` | — | mutexes/atomics; `-race` tests pass |
| Numeric validation | Clean | — | — | clamps, ranges, parse checks |
| Transaction boundaries | **Finding (low)** | — | F-16 | count/insert not atomic |
| Inconsistent authorization | **Finding** | `hunt-session` | F-01 | web vs desktop revocation asymmetry |
| Pricing calculations | N/A | `hunt-fintech-graphql` | — | no payments |
| Resource exhaustion | **Finding** | `hunt-business-logic` | F-02, F-03, F-17 | sockets/cache/entitlement scan |
| Idempotency | Clean | — | — | upserts, unique constraints, CAS |
| Database consistency | Clean | — | — | FKs, cascades, CHECK, unique |

### 3.11 Logging, monitoring and error handling

| Category | Status | Skills | Findings | Verification |
|---|---|---|---|---|
| Panic containment | Clean | — | — | `Recover` logs stack, returns generic 500 |
| Error mapping | **Finding (low)** | `hunt-exceptional-conditions` | F-19 | details outside production |
| Log injection | Clean | — | — | structured slog; UA truncated |
| Sensitive log fields | **Finding (low)** | `evidence-hygiene` | F-20 | email in recovery log; no tokens/passwords |
| Audit trail | Partial | - | - | request log + party/job rows; no admin audit log |
| Metrics exposure | Clean (defaults) | — | — | loopback; documented |
| Health endpoint | Clean | — | — | reports status/storage/db only |

### 3.12 Mobile, cloud-native, and other asset classes

| Category | Status | Skills | Reason |
|---|---|---|---|
| Android APK | N/A | `apk-redteam-pipeline` | no Android app in repo |
| iOS IPA | N/A | `ios-redteam-pipeline` | no iOS app in repo |
| Kubernetes | N/A | `hunt-k8s` | no K8s manifests |
| gRPC | N/A | `hunt-grpc` | no gRPC service |
| SAML/ASP.NET/SharePoint | N/A | `hunt-saml`, `hunt-aspnet`, `hunt-sharepoint` | not the stack |
| Laravel/Next.js/Node/Spring | N/A | respective skills | not the stack |
| Smart contracts | N/A | `web3-audit`, `meme-coin-audit` | no chain code |
| Cloud IAM chains | N/A | `cloud-iam-deep` | static S3 keys only; no role chain |
| M365/Okta/Entra | N/A | respective skills | no such IdP |
| VPN/VMware | N/A | respective skills | no such appliance |
| RAG/vector store | N/A | `hunt-rag-vector` | no vector DB |
| Frontend/DOM | N/A | `hunt-dom`, `hunt-spa-api` | frontend not in repo |

## 4. Tools executed

| Tool | Command | Result |
|---|---|---|
| Go build/vet | `go vet ./...` | clean |
| Go tests | `go test ./...` | all packages pass |
| Module integrity | `go mod verify` | all modules verified |
| Vulnerability scan | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | 2 reachable + 1 package-level |
| Secret scan | `git grep` regex over tracked files; history filename scan | no tracked secret |
| `gosec` | `go run github.com/securego/gosec/v2/cmd/gosec@latest ...` | **interrupted / unavailable** — rerun in CI |
| Semgrep / gitleaks / Trivy / Checkov / pip-audit / npm audit | not installed | **not run** |
| Runtime/dynamic testing | none | not authorised / no running instance |

## 5. Files reviewed

All `.go` files under `cmd/` and `internal/` (107), plus:

- `migrations/0001_core.sql`, `0002_watch_parties.sql`, `0003_downloads.sql`, `embed.go`
- `go.mod`, `go.sum`, `.golangci.yml`, `.gitignore`, `.gitattributes`, `.dockerignore`,
  `.editorconfig`, `.env.example`
- `Dockerfile`, `docker-compose.yml`
- `deploy/nginx/nginx.conf`, `deploy/prometheus/prometheus.yml`,
  `deploy/grafana/provisioning/*`, `deploy/grafana/dashboards/*`
- `.github/workflows/ci.yml`, `.github/dependabot.yml`, issue/PR templates
- `SECURITY.md`, `README.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `LICENSE`, `NOTICE`
- `Makefile`, `scripts/*.py`

## 6. Documented negatives (do not re-test without a reason)

1. SQL injection — parameterised throughout; no dynamic identifiers from user input.
2. Command injection — no shell; ffmpeg args are a slice; stream indexes are integers.
3. Path traversal — `LocalPath` + symlink re-check + HLS regex + importer guards.
4. SSRF — no user-controlled URL sink; all provider endpoints config-fixed.
5. JWT `alg:none` / key confusion — algorithm pinned to HS256; HMAC constant-time.
6. Admin route missing `RequireAdmin` — none; all covered.
7. IDOR on user data/downloads/sessions — ownership in SQL and handlers.
8. CSRF on state-changing routes — SameSite=Lax + JSON + non-GET; no state-changing GET.
9. CSWSH — Origin allowlist; browser cannot omit Origin.
10. GraphQL mutation via the proxy — the query text must contain the literal `mutation`, which
    is rejected; variables cannot introduce a mutation.
11. Webhook secret timing — constant-time compare.
12. OAuth state replay — cache `Take` is atomic and the token is consumed once.
13. Open redirect in the OAuth return — exact origin allowlist.
14. Mass assignment on profile/admin — DTOs + `DisallowUnknownFields`.
15. Path traversal in HLS segments — strict regex.
16. Password hash downgrade — parameters are read from the stored hash; rehash on login.
17. Session token predictability — 32/48/16 random bytes from `crypto/rand`.
18. Token-at-rest exposure — SHA-256 only.
19. Database cascade on account delete — verified in schema.
20. Container root — `USER animind`.

## 7. Unreviewed / not assessable

| Area | Why | What would close it |
|---|---|---|
| Frontend XSS sinks | Website/desktop player not in repo | Renderer review; sanitize subtitle/VTT/`anime_data`/`thumbnail_url` |
| Bucket policy / IAM | Cloud state not in repo | `aws s3api get-bucket-policy`, Block Public Access, key scope |
| TLS terminator | Not in repo | Verify HTTPS, HSTS, secure cookies, cipher policy |
| Host firewall / network | Not in repo | Confirm 3001/3002/5432/6379 are not publicly reachable |
| DB role privileges | Not in repo | `\du`, least-privilege grants, `sslmode=verify-full` |
| ffmpeg/torrent parser CVEs | Version-dependent and inherently untrusted input | SBOM + patch cadence + sandboxing |
| Live behaviour | No running instance | Staging test of the regression suite |
| Mobile clients | Not in repo | Separate mobile audit |

## 8. Sign-off

Coverage is complete for the repository contents. The unresolved risks are either deployment
controls (TLS, bucket policy, firewall), absent components (frontend, mail transport), or inherent
to processing untrusted media. The two High findings and the three Medium findings are
remediable without architectural change; the patches are in `REMEDIATION_PLAN.md`.
