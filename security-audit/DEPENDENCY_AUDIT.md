# Dependency & Supply-Chain Audit

**Snapshot:** working tree at `bf58946` + uncommitted changes (`go 1.27.4`, upgraded torrent and
`golang.org/x/*` versions)
**Date:** 2026-11-25
**Scope:** Go modules, GitHub Actions, container images, Python helper scripts, published
artefacts.

---

## 1. Manifest and lockfile posture

| Item | Status |
|---|---|
| Go module manifest | `go.mod` present, `go 1.27.4` |
| Go lockfile | `go.sum` present (53 538 bytes in the working tree) |
| Vendor directory | absent (module proxy + checksum DB trusted) |
| `go mod verify` | **pass** — "all modules verified" |
| npm / pip / Maven / Gradle | none |
| Python helper scripts | `scripts/*.py` import only the standard library; no requirements manifest required |
| SBOM | not generated |
| Artefact signing / provenance | not implemented |

All dependency versions are pinned in `go.mod`/`go.sum`; the working tree upgraded several
transitive modules, which is good for freshness but should be committed after the test suite is
green.

## 2. Direct dependencies

| Module | Version | Purpose | Notes |
|---|---|---|---|
| `github.com/anacrolix/torrent` | `v1.61.1-0.20260911233437-76452a2c8a2f` | BitTorrent engine | Pseudo-version on a post-release commit; Dependabot cannot reason about it like a semver tag. Pin to a tagged release when one exists. |
| `github.com/aws/aws-sdk-go-v2` (+config, credentials, s3) | v1.47.1 / v1.33.6 / v1.20.6 / v1.113.4 | S3 presigning and object access | Current; no advisory found |
| `github.com/go-chi/chi/v5` | v5.3.2 | HTTP router | Current |
| `github.com/google/uuid` | v1.6.0 | UUID parsing | Current |
| `github.com/gorilla/websocket` | v1.5.3 | Watch-party sockets | **GO-2026-6278** (see §3) |
| `github.com/jackc/pgx/v5` | v5.11.0 | PostgreSQL driver/pool | Current |
| `github.com/prometheus/client_golang` | v1.24.1 | Metrics | Current |
| `github.com/redis/go-redis/v9` | v9.22.0 | Optional shared cache/limiter | Current |
| `github.com/robfig/cron/v3` | v3.0.1 | Scheduler | Current |
| `golang.org/x/crypto` | `v0.58.1-0.20261009181718-2f1f834330b2` | argon2id | Pseudo-version; pin to a release tag when available |
| `golang.org/x/sync` | v0.24.0 | Concurrency helpers | Current |
| `golang.org/x/sys` | v0.49.0 | Platform syscalls | Current |
| `golang.org/x/text` | v0.43.0 | Text | Current |
| `golang.org/x/time` | v0.14.0 | Rate limiter | Current |

Notable indirect modules: `go.opentelemetry.io/otel* v1.42.0` (via the torrent stack),
`github.com/pion/*` (WebRTC/DTLS/STUN for torrent), `modernc.org/sqlite` (torrent metadata
storage), `github.com/prometheus/*`.

## 3. `govulncheck` results

Command:

```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Result: **2 reachable vulnerabilities from 2 modules, 1 package-level vulnerability**.

| ID | Module | Found | Fixed | Reachability | Assessment |
|---|---|---|---|---|---|
| GO-2026-6278 | `github.com/gorilla/websocket` | v1.5.3 | v1.5.4-0.20240701034025-d67f41855da4 | Reachable through the syncplay hub (`Upgrade`, `ReadMessage`, `WriteMessage`, `WriteControl`) and through anacrolix's tracker/client code | Weak PRNG for WebSocket masking keys. Masking is traffic obfuscation, not authentication; no direct ATO path. **Upgrade anyway.** |
| GO-2026-6974 | `go.opentelemetry.io/otel/trace` | v1.42.0 | v1.47.0 | Reachable indirectly (torrent/storage call paths) | `AttributeValueLengthLimit` bypass via U+FFFD. Only matters if telemetry is exported; the app does not configure an exporter. **Upgrade to keep the graph clean.** |
| GO-2026-6975 | `go.opentelemetry.io/otel` | v1.42.0 | v1.47.0 | Package imported, symbol not called | Baggage memory-amplification. No exploitable call path found. **Upgrade with the others.** |

No vulnerability was found in `pgx`, `chi`, `aws-sdk-go-v2`, `go-redis`, `prometheus/client_golang`,
`robfig/cron`, `golang.org/x/crypto`, or `anacrolix/torrent` itself at the audited revision.

### Remediation commands

```bash
go get github.com/gorilla/websocket@latest
go get go.opentelemetry.io/otel@v1.47.0 \
      go.opentelemetry.io/otel/trace@v1.47.0 \
      go.opentelemetry.io/otel/metric@v1.47.0
go mod tidy
go test ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Do not blindly accept the pseudo-version for `gorilla/websocket` if `@latest` resolves to a
pre-release; pin the fixed commit/tag and record why.

## 4. GitHub Actions and CI

Workflow: `.github/workflows/ci.yml`

| Control | Status | Comment |
|---|---|---|
| Trigger safety | **Good** | `push`/`pull_request`/`workflow_dispatch`; no `pull_request_target`, no `workflow_run` |
| Token permissions | **Good** | top-level `permissions: contents: read` |
| Secrets exposure | **Good** | no repository secrets referenced; service containers use ephemeral local credentials |
| Code execution from forks | **Safe** | no privileged trigger checks out PR code with secrets |
| Dependency caching | Standard | `actions/setup-go` cache; no untrusted cache write followed by privileged consume |
| Action pinning | **Weak** | `actions/checkout@v4`, `actions/setup-go@v5` are mutable tags |
| SAST / vuln scan | **Missing** | no `govulncheck`, gosec, Semgrep, CodeQL, or secret scan |
| SBOM / signing | **Missing** | no CycloneDX/SPDX, cosign, or SLSA provenance |
| Test rigour | **Good** | `go test -race -p 1 ./...` plus a skip guard for missing Postgres/Redis |
| Docker build | Present | image is built but not scanned or signed |

### Recommendation

1. Add a `security` job with `govulncheck`, `gosec` (or Semgrep), and `gitleaks` on `push` and
   `pull_request`. Fail on reachable findings and on new secrets.
2. SHA-pin `actions/checkout`, `actions/setup-go`, and any future action; let Dependabot update
   the SHA pins.
3. Add `docker scout`/Trivy image scanning and a CycloneDX SBOM artefact.
4. Keep the existing read-only permissions and the no-secrets property; do not add a
   `pull_request_target` workflow for labelling or commenting without a separate, unprivileged
   workflow.

## 5. Container images

| Image | Tag | Assessment |
|---|---|---|
| `golang:1.27-alpine` (build stage) | tag | Current toolchain; use `golang:1.27-alpine` digest for reproducibility |
| `alpine:3.20` (runtime) | tag | **Released May 2024; the two-year support window ends around May 2026.** Verify the current support status and move to a supported Alpine release. It carries ffmpeg, ca-certificates and tzdata, so stale packages are a real exposure. |
| `nginx:1.27-alpine` | tag | Development compose only; 1.27 is a mainline branch. Pin a stable/digest for deployment. |
| `postgres:16-alpine` | tag | Development compose only; Postgres 16 is supported. Pin digest. |
| `redis:7-alpine` | tag | Development compose only; Redis 7 is supported. Pin digest. |
| `prom/prometheus:v2.53.0` | semver tag | Older release; development dashboard only. Update. |
| `grafana/grafana:11.2.0` | semver tag | Older release; development dashboard only, default admin password. Update and set credentials. |

Dockerfile positives: multi-stage build, `CGO_ENABLED=0`, `-trimpath`, non-root `animind` user,
explicit healthcheck, no build tools in the runtime image. Negatives: no `--read-only`,
`no-new-privileges`, capability drop, or resource limits (those belong in compose/run config).

## 6. Python helper scripts

`scripts/e2e_verify.py`, `verify_cache.py`, `verify_downloads.py`, `verify_media.py`.

- Standard-library only (`http.client`, `urllib`, `subprocess`, `json`, `hashlib`, …).
- No third-party packages, so no `pip-audit` surface.
- They spawn local servers/commands for verification. Treat them as developer tooling, not
  production entry points; do not run them against a remote host without authorisation.

## 7. Secrets and artefacts

| Check | Result |
|---|---|
| Tracked `.env` files | none (`git ls-files` shows only `.env.example`) |
| `.env`/keys ignored | yes (`.gitignore`: `.env`, `.env.local`, `*.key`, `*.pem`) |
| Hardcoded credentials in Go | none found outside test constants |
| High-entropy token regex over tracked files | no matches |
| Git history file-add scan for `.env`/keys | no additions found |
| `build/` directory | git-ignored; contains binaries, `.torrent.db*` and logs. Logs were inspected for secret patterns: only a user id and `method=password`; no token/secret value. Still, treat the folder as sensitive and clean it before sharing. |
| `library/` | git-ignored media tree; may contain copyrighted content and should never be committed |

## 8. Supply-chain risk summary

| Risk | Likelihood | Impact | Priority |
|---|---|---|---|
| Stale Alpine runtime packages (ffmpeg, TLS roots) | Medium | High (parser/RCE class) | P1 |
| Mutable action tags compromised upstream | Low | High (CI code execution) | P1 (cheap to fix) |
| Go module compromise | Low | High | P2 (checksums + Dependabot already mitigate) |
| Pseudo-version dependencies drift | Medium | Low | P2 |
| Container image drift from `latest`-style tags | Medium | Medium | P2 (pin digests) |
| No SBOM/signing | Medium | Medium (response time) | P3 |
