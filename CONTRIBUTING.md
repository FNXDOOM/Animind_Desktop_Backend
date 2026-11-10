# Contributing

Thanks for taking an interest. This is a personal anime streaming backend — one Go
process, one Postgres, optionally Redis — and it is meant to be self-hostable in an
afternoon. Changes that keep that true are the ones most likely to land.

## Before you start

- **Requirements:** Go 1.27+, PostgreSQL 14+. `ffmpeg` and `ffprobe` only if you intend to
  run `STORAGE_MODE=local`, which is also the only mode where subtitles, audio variants and
  HLS exist.
- `cp .env.example .env`, then set `DATABASE_URL` and `DESKTOP_TOKEN_SIGNING_KEY` (32+
  characters). The server refuses to start without them and names the variable it objected
  to; that behaviour is deliberate and load-bearing, so don't work around it.
- `make build && make run` migrates the database and listens on `:3001`.
- `make docker-up` brings up the whole stack, including Postgres, Redis, nginx, Prometheus
  and Grafana, if you would rather not install any of them.

Open an issue before anything that changes a route, a payload field, or the watch-party
protocol. The wire contract is consumed by two shipped clients, and those conversations go
much better before code exists than after.

## Running the checks

```bash
make test        # unit tests. Integration suites skip when their service is absent.
make vet
make fmt
make cover
```

Green-but-skipped is not green. Two suites only run when told where the services are, and
they are the two that prove the interesting parts:

```bash
docker compose up -d db redis
export DATABASE_URL=postgres://animind:animind@localhost:5432/animind?sslmode=disable
export REDIS_URL=redis://localhost:6379/0
make test-cache     # -p 1: the DB-backed suites share tables and would race
```

Then the live harnesses, which check a *running server* rather than functions. These are
what found the defects that type-checking missed, so please run them if you touched
anything user-visible:

```bash
python3 scripts/e2e_verify.py   --base http://localhost:3001   # auth, catalog, playback, pairing, admin
python3 scripts/verify_media.py --base http://localhost:3001   # subtitles, variants, HLS; needs local mode
python3 scripts/verify_cache.py                                # Redis keys, shared windows, dashboards
```

## What a complete change looks like

A pull request that adds or changes an endpoint is expected to carry four things. Not
policy for its own sake: each one has caught a real defect.

1. **Code**, obviously.
2. **A test in the matching package.** Route and payload changes belong in `internal/httpx`;
   SQL in `internal/repository` (which needs a database); gate behaviour in
   `internal/syncplay/gate_test.go`, which deliberately runs with no database so the
   protocol has a test on machines without one.
3. **A `docs/openapi.yaml` entry.** CI diffs the route table against the spec, because
   comparing paths instead of methods hides a missing verb.
4. **A line in `CHANGELOG.md`** under `Unreleased`.

## Conventions worth knowing

**Dependencies point one way.** `httpx` and `scheduler` are imported by nothing but
`cmd/server`. `repository` holds every SQL statement and knows nothing about HTTP. `cache`
imports only `config`. A pull request that adds an arrow in the other direction needs a
reason in its description.

**Ownership comes from the session.** No endpoint accepts a `user_id` from a client. Every
user-data query filters `user_id = $n::uuid` using the authenticated principal. If your
change reads user data, that is the pattern; there are no exceptions to negotiate.

**Fail fast at startup.** Anything that can fail permanently should fail before the
listener opens. Do not add a lazily-initialised dependency that 500s on first use.

**Comments explain why, not what.** If the next three lines say what the code does, delete
the comment. If they say why a constraint exists, what invariant must hold, or which bug a
weird-looking line prevents, keep it and keep it short — one to three lines. This project
prefers a few dense reasons to a paragraph per function.

**Line endings are LF.** `.gitattributes` enforces it. If your editor writes CRLF,
`gofmt` will report files that are not actually misformatted.

## Style

- `gofmt` output is final; CI runs the check.
- Errors are wrapped with `%w` and compared with `errors.Is`, so callers can classify them.
- Handlers answer through `WriteJSON` / `WriteError*` so the payload shape stays uniform.
- Secrets are never logged. Neither are playback tickets: they are bearer-equivalent for
  their lifetime.

## Pull requests

Small and singular. `fix(syncplay): close the gate when a peer leaves` beats a branch that
also tidies three packages. Use Conventional Commit subjects (`feat`, `fix`, `test`,
`docs`, `chore`) with a scope, because `CHANGELOG.md` is organised the same way.

Describe what changes for someone running this, and how you verified it. If a check skipped
because a service was missing, say so instead of implying it passed.

## Reporting something rather than fixing it

Bugs: open an issue with the exact reproduction and which mode you were in — `local` and
`s3` are genuinely different programs in places. Security issues: see
[SECURITY.md](SECURITY.md), and please do not open a public issue first.
