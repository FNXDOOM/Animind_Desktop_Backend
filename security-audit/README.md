# Security Audit — Animind Backend (Go)

Internal defensive security audit of the local repository, performed 2026-11-25 with the
Claude-BugHunter skill set. No source or configuration was modified by the audit itself;
nothing was deployed or attacked.

> **Remediation update:** the P0/P1/P2 fixes have since been applied to the working tree and
> verified locally. See `REMEDIATION_STATUS.md` for the per-finding status, the changed files
> and the verification log. The finding text below is the pre-fix assessment.

## Read in this order

1. **`EXECUTIVE_SUMMARY.md`** — verdict, severity counts, the three fixes to make first.
2. **`FINAL_SECURITY_REPORT.md`** — method, coverage, verified controls, residual risk.
3. **`SECURITY_FINDINGS.md`** — every finding with evidence, impact, CWE/CVSS and patch.
4. **`REMEDIATION_PLAN.md`** — prioritised patches, acceptance criteria, rollout risk.
5. **`REGRESSION_TESTS.md`** — proposed Go tests, fail-before/pass-after.

## Reference documents

| File | Contents |
|---|---|
| `PROJECT_ARCHITECTURE.md` | components, data flow, trust boundaries, auth/session model |
| `THREAT_MODEL.md` | assets, actors, abuse cases, attack trees, assumptions |
| `SKILL_COVERAGE.md` | installed skill inventory, applicability, finding provenance |
| `DEPENDENCY_AUDIT.md` | direct modules, `govulncheck`, CI/actions, images, secrets |
| `INFRASTRUCTURE_AUDIT.md` | Docker, compose, nginx, metrics, DB/Redis, S3, host |
| `BUSINESS_LOGIC_AUDIT.md` | 40 business rules, state machines, quota/session/party logic |
| `AUDIT_CHECKLIST.md` | every security category with status, skills, findings |
| `EVIDENCE.md` | redacted commands and code excerpts behind the findings |

## Headline

- **0 Critical**, **2 High**, **3 Medium**, **11 Low**, **6 Informational**.
- Strongest areas: SQL scoping, path containment, token cryptography, OAuth state, admin
  middleware, password hashing.
- Highest-priority fix: revoke desktop sessions on password change/reset/email change
  (`F-01`), because a stolen web session can be converted into a long-lived desktop credential
  that survives the victim's remediation.
- Then: bound the unauthenticated socket surface (`F-02`) and the AniList cache (`F-03`), and
  make the production configuration fail closed (`F-05`).

## Reproduce the static checks

```powershell
go vet ./...
go test ./...
go mod verify
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
