# Security Audit — Animind Backend (Go)

Defensive security audit of this repository, performed 2026-11-25 with the Claude-BugHunter
skill set. Findings were remediated in commit `ede8d1a`; nothing was deployed or attacked.

## Read in this order

1. **`EXECUTIVE_SUMMARY.md`** — verdict, severity counts, the fixes that were made first.
2. **`FINAL_SECURITY_REPORT.md`** — method, coverage, verified controls, residual risk.
3. **`SECURITY_FINDINGS.md`** — every finding with evidence, impact, CWE/CVSS and patch.
4. **`REMEDIATION_STATUS.md`** — per-finding result after the fixes, plus the verification log.
5. **`REMEDIATION_PLAN.md`** / **`REGRESSION_TESTS.md`** — remediation design and the tests that pin it.

## Reference documents

| File | Contents |
|---|---|
| `PROJECT_ARCHITECTURE.md` | components, data flow, trust boundaries, auth/session model |
| `THREAT_MODEL.md` | assets, actors, abuse cases, attack trees, assumptions |
| `SKILL_COVERAGE.md` | installed skill inventory, applicability, finding provenance |
| `DEPENDENCY_AUDIT.md` | direct modules, `govulncheck`, CI/actions, images, secrets |
| `INFRASTRUCTURE_AUDIT.md` | Docker, compose, nginx, metrics, DB/Redis, S3, host |
| `BUSINESS_LOGIC_AUDIT.md` | business rules, state machines, quota/session/party logic |
| `AUDIT_CHECKLIST.md` | every security category with status, skills, findings |
| `EVIDENCE.md` | redacted commands and code excerpts behind the findings |

## Headline (pre-fix assessment)

- **0 Critical**, **2 High**, **3 Medium**, **11 Low**, **6 Informational** — all code-level
  findings are fixed; see `REMEDIATION_STATUS.md`.
- Strongest areas: SQL scoping, path containment, token cryptography, OAuth state, admin
  middleware, password hashing.
- Highest-impact fix: desktop sessions are now revoked on password change/reset/email change
  (`F-01`), and refresh-token replay revokes the token family (`F-11`).

## Reproduce the static checks

```bash
go vet ./...
go test ./...
go mod verify
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
