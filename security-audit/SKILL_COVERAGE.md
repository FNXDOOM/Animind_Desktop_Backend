# Skill Coverage Matrix

## 1. How skills were verified

Installed skill inventory was read from both skill roots on this machine:

- `C:\Users\gudiy\.agents\skills` — 91 directories, each containing `SKILL.md`
- `C:\Users\gudiy\.claude\skills` — mirrored set (same names, plus `graphify`)

**Status legend**

| Status | Meaning |
|---|---|
| **Loaded** | Full `SKILL.md` content was loaded into the working context with the skill tool |
| **Applied** | Installed description and category checklist were applied during the review; the full file was not loaded (context conservation), and the category was covered by direct source review |
| **N/A** | The technology, protocol, asset class or engagement type is absent from this repository; reason given |
| **Blocked** | Skill applies, but a required external tool/service is unavailable |

**Engagement mode (bb-methodology PART 0):** internal defensive audit, not bug bounty, red team
or WAPT. Findings are therefore classified by control failure and business impact, with CVSS
where evidence supports it, and hygiene findings are retained only when they change a security
property.

## 2. Core workflow and validation skills

| Skill | Status | Use in this audit | Findings |
|---|---|---|---|
| `bb-methodology` | **Loaded** | Mode confirmation, 5-phase mapping, false-positive discipline, evidence bar | process |
| `triage-validation` | **Loaded** | 7-Question Gate, Pre-Severity Gate, never-submit list, CVSS | F-01…F-22 |
| `evidence-hygiene` | **Loaded** | Redaction rule: no live secrets, hashes only, no PII in this report | all |
| `report-writing` | **Loaded** | Title formula, impact-first structure, severity calibration | all |
| `bug-bounty` | **Loaded** | Orchestrator checklist, verified-control list, A→B chains | F-01 chain, F-04 |
| `bb-local-toolkit` | **Loaded** | Local tool inventory; no optional scanners installed | `DEPENDENCY_AUDIT.md` |
| `exploit-completion` | **Loaded** | Redirect/expected-outcome discipline (applied to OAuth and download flows) | F-04, F-10 |
| `security-review` | Applied | Systematic OWASP-style source review categories | all |
| `code-review` | Applied | Standards/spec lens on auth and download code | F-01, F-16 |
| `security-arsenal` | Applied | Payload/checklist reference for control validation (not weaponised) | verified controls |
| `redteam-mindset` | Applied | Do-not-stop coverage discipline; documented negatives | `AUDIT_CHECKLIST.md` |
| `find-skills` | Applied | Confirmed the installed skill set before selecting skills | this file |

## 3. Vulnerability-class skills

| Skill | Status | Application / reason | Findings |
|---|---|---|---|
| `hunt-auth-bypass` | **Loaded** | Auth entry points, legacy-protocol matrix (none present), JWT/authz separation | F-01, F-04, F-06, F-10 |
| `hunt-ato` | **Loaded** | 11-path ATO taxonomy; checked reset, email change, JWT, pre-account, passwordless | F-01, F-04, F-10, F-11 |
| `hunt-idor` | **Loaded** | Object references in `/api/me/*`, downloads, sessions, HLS, rooms | none confirmed (F-08 partial) |
| `hunt-brute-force` | **Loaded** | Four rate-limit states, enumeration, token entropy, distributed bypass | F-06, F-09 |
| `hunt-session` | **Loaded** | Fixation, logout/password-change invalidation, cookie attributes, refresh rotation, JWT-as-session | F-01, F-11, F-17 |
| `hunt-websocket` | **Loaded** | CSWSH, per-message auth, namespace/room authz, pre-auth resource use | F-02, F-09 |
| `hunt-captcha-bypass` | **Loaded** | No CAPTCHA exists in the product; client-only gate pattern not present | none |
| `hunt-api-misconfig` | Applied | Mass assignment (`DisallowUnknownFields`), verb tampering, prototype pollution (Go), JWT handling | none confirmed |
| `hunt-csrf` | Applied | SameSite=Lax, JSON bodies, no state-changing GET, OAuth state | none confirmed |
| `hunt-cors` | Applied | Origin reflection, wildcard + credentials, preflight | F-12 |
| `hunt-jwt-crypto` | Applied | `alg:none`, key confusion, `kid`/`jku`, weak HMAC | none confirmed |
| `hunt-oauth` | Applied | `state`, `redirect_uri`, PKCE relevance, code leakage, account linking | F-04 |
| `hunt-forgot-password` | Applied | Enumeration, token in response, replay, binding, rate limit | F-01, F-14, F-20 |
| `hunt-mfa-bypass` | Applied | No MFA implemented; step-up absence noted for destructive actions | F-01 |
| `hunt-ssrf` | Applied | All outbound URL construction reviewed | none confirmed |
| `hunt-sqli` | Applied | Every repository statement reviewed | none confirmed |
| `hunt-nosqli` | Applied | PostgreSQL only; no Mongo/Couch/Redis query surface | N/A (reason) |
| `hunt-xss` | Applied | API emits JSON; stored content sinks owned by absent frontend | conditional note |
| `hunt-dom` | N/A | No frontend/DOM in this repository | — |
| `hunt-html-injection` | Applied | JSON responses + `nosniff`; no HTML render | none |
| `hunt-ssti` | Applied | No template engine renders request data | none |
| `hunt-rce` | Applied | `exec.Command` argument review, ffmpeg, torrent parser | none confirmed |
| `hunt-lfi` | Applied | File include/traversal paths; `LocalPath`, HLS, subtitles | none confirmed |
| `hunt-file-upload` | Applied | No multipart upload endpoint; torrent import/subtitle writes reviewed | none confirmed |
| `hunt-xxe` | Applied | Torznab XML parser (`encoding/xml`, no external entities) | none |
| `hunt-deserialization` | Applied | JSON-only decode; no pickle/Java/PHP/.NET/Ruby deserialisation | none |
| `hunt-ldap` | N/A | No LDAP/AD integration | — |
| `hunt-exceptional-conditions` | Applied | Error mapping, `safeDetail`, panic recovery | F-05, F-19 |
| `hunt-business-logic` | Applied | Quotas, state machines, entitlements, session lifecycle | F-01, F-16 |
| `hunt-race-condition` | Applied | Download quota, HLS cap, state transitions, token consumption | F-16 |
| `hunt-cache-poison` | Applied | AniList document cache key/normalisation and byte budget | F-03 |
| `hunt-graphql` | Applied | AniList proxy only; no local schema/resolvers | none |
| `hunt-fintech-graphql` | N/A | No ledger, balance, transfer, payment or KYC model | — |
| `hunt-host-header` | Applied | `PublicIssuer` is config-driven; no reset mailer builds links from Host | none confirmed |
| `hunt-open-redirect` | Applied | `sanitizeReturnTarget` origin allowlist | none confirmed |
| `hunt-http-smuggling` | Applied | nginx → Go; no CL/TE disagreement in shipped config | none confirmed |
| `hunt-clickjacking` | Applied | `X-Frame-Options: DENY` | none confirmed |
| `hunt-web-cache` (via `hunt-cache-poison`) | Applied | API sends `no-store`; media caches private | none confirmed |
| `hunt-shadow-api` | Applied | No versioned/legacy API surface found | none |
| `hunt-spa-api` | N/A | No SPA/JS bundle in this repository | — |
| `hunt-source-leak` | Applied | Git history, tracked files, CI, build artefacts, `.env` handling | F-18 (CI only) |
| `hunt-grpc` | N/A | No gRPC/protobuf service | — |
| `hunt-k8s` | N/A | No Kubernetes manifests or exposed kubelet/etcd | — |
| `hunt-subdomain` | N/A | No subdomain assets in scope; cookie-domain discussion only | F-17 |
| `hunt-tls-network` | Applied | nginx/compose TLS, HSTS, cookie `Secure`, DMARC out of scope | F-05, F-12, F-17 |
| `hunt-cloud-misconfig` | Applied | S3 client, presign TTL, bucket handling, metrics exposure | F-05, Info |
| `hunt-cicd` | Applied | GitHub Actions triggers/permissions/pinning; Dependabot | F-18 |
| `hunt-llm-ai` | Applied | OpenRouter scanner fallback: prompt content, schema, confidence, no tools | none confirmed |
| `hunt-rag-vector` | N/A | No vector store/RAG | — |
| `hunt-saml` | N/A | No SAML/SSO | — |
| `hunt-mfa-bypass` | Applied | No MFA factor to bypass | — |
| `hunt-ntlm-info` | N/A | No IIS/SharePoint/Exchange | — |
| `hunt-aspnet` | N/A | Not ASP.NET | — |
| `hunt-laravel` | N/A | Not PHP/Laravel | — |
| `hunt-nextjs` | N/A | Not Next.js | — |
| `hunt-nodejs` | N/A | Not Node.js | — |
| `hunt-springboot` | N/A | Not Spring Boot | — |
| `hunt-sharepoint` | N/A | No SharePoint | — |
| `hunt-misc` | Applied | General attack-surface checklist and chaining lens | all |
| `hunt-dispatch` | N/A | Single known local target; no platform dispatch needed | — |

## 4. Cloud / identity / infrastructure skills

| Skill | Status | Reason |
|---|---|---|
| `cloud-iam-deep` | N/A | No AWS/Azure/GCP IAM credential or role chain in the repository; S3 static keys only |
| `m365-entra-attack` | N/A | No Microsoft identity platform |
| `okta-attack` | N/A | No Okta tenant |
| `enterprise-vpn-attack` | N/A | No VPN appliance |
| `vmware-vcenter-attack` | N/A | No VMware surface |
| `apk-redteam-pipeline` | N/A | No Android application |
| `ios-redteam-pipeline` | N/A | No iOS application |
| `web3-audit` | N/A | No smart contracts |
| `meme-coin-audit` | N/A | No token contract |
| `workers-best-practices` | N/A | No Cloudflare Workers |
| `better-auth-best-practices` | N/A | Custom auth stack, not Better Auth |
| `supply-chain-attack-recon` | Applied | `go.mod`/`go.sum`, Dependabot, Actions, base images, secret scanning |
| `web2-recon` | N/A | Local source audit; no external asset discovery authorised |
| `offensive-osint` | N/A | No external reconnaissance performed |
| `osint-methodology` | N/A | No OSINT collection |
| `recon-scope-triage` | N/A | No ASM dataset to triage |
| `mid-engagement-ir-detection` | N/A | No live monitored target |
| `performance` | Applied (secondary) | Only where resource limits affect availability (F-02, F-03, F-16) |
| `frontend-design` | N/A | No frontend |
| `improve-codebase-architecture` | N/A | Not a refactoring engagement |
| `systematic-debugging` | N/A | No failing test/crash investigation required |
| `grill-me` | N/A | Not requested |
| `graphify` | N/A | Not requested; repository map produced directly |
| `bugcrowd-reporting` | N/A | Not a Bugcrowd submission; internal audit format used |
| `redteam-report-template` | N/A | Internal audit uses the deliverable set in this directory |

## 5. Finding → skill provenance

| Finding | Primary skills |
|---|---|
| F-01 Desktop sessions survive password/reset/email change | `hunt-session`, `hunt-ato`, `hunt-auth-bypass`, `hunt-mfa-bypass` |
| F-02 Unauthenticated socket connection exhaustion | `hunt-websocket`, `hunt-brute-force`, `hunt-business-logic` |
| F-03 AniList cache unbounded bytes | `hunt-cache-poison`, `hunt-api-misconfig`, `performance` |
| F-04 Unverified signup + Google email linking | `hunt-ato`, `hunt-auth-bypass`, `hunt-oauth` |
| F-05 Insecure production defaults / placeholder secrets | `hunt-tls-network`, `hunt-exceptional-conditions`, `hunt-cloud-misconfig` |
| F-06 Login/signup user enumeration | `hunt-brute-force`, `hunt-auth-bypass` |
| F-07 IPv6 `ClientIP` parsing | `hunt-brute-force`, `hunt-exceptional-conditions` |
| F-08 HLS session ownership | `hunt-idor`, `hunt-websocket` (shared-resource authz) |
| F-09 Watch-party room-code brute force | `hunt-brute-force`, `hunt-websocket` |
| F-10 Bridge token valid after logout | `hunt-session`, `hunt-ato` |
| F-11 Refresh rotation without reuse detection | `hunt-session`, `hunt-jwt-crypto` |
| F-12 Wildcard CORS with credentials | `hunt-cors`, `hunt-csrf` |
| F-13 Vulnerable dependencies | `supply-chain-attack-recon`, `hunt-cicd` |
| F-14 Password reset delivery absent | `hunt-forgot-password`, `hunt-ato` |
| F-15 Cookie prefix / cookie tossing | `hunt-session`, `hunt-subdomain` |
| F-16 Download quota TOCTOU | `hunt-race-condition`, `hunt-business-logic` |
| F-17 Entitlement lookup scans all users | `hunt-business-logic`, `performance` |
| F-18 CI/CD and image pinning | `hunt-cicd`, `supply-chain-attack-recon` |
| F-19 Non-production error detail exposure | `hunt-exceptional-conditions` |
| F-20 PII in recovery log line | `evidence-hygiene`, `hunt-forgot-password` |
| F-21 Dead/unused socket state | `code-review` |
| F-22 Web sessions record no IP/user agent | `hunt-session`, `code-review` |

## 6. Skills deliberately not exercised

The following were considered but not run because the corresponding test would be destructive,
external, or outside the owner's authorisation:

- live credential brute force / password spraying (`hunt-brute-force` phase 2b full keyspace);
- OOB SSRF/XXE callbacks (`hunt-ssrf`, `hunt-xxe`) — no user-controlled URL sink exists;
- socket brute force against a live room (`hunt-websocket`) — local code analysis instead;
- dependency exploitation (`govulncheck` identifies reachability, not exploitability);
- external reconnaissance (`web2-recon`, `offensive-osint`) — local-only scope.

Documented negatives are recorded in `AUDIT_CHECKLIST.md` §6 so a later reviewer does not repeat
the same work.
