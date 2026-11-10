## What changes, for someone running this

One or two sentences on the visible difference. Not a list of files — a reader deciding
whether to pull this should know what changes about their server.

## Was this discussed first?

- [ ] Yes — issue or discussion: (link)
- [ ] No, and it is small enough not to need one
- [ ] No, but it changes a route, a payload field or the watch-party protocol

That third box should not be ticked. Those shapes are read by shipped clients; start the
conversation first.

## How I verified it

Put the command and its real output, including the parts that did not run.

```
make vet
make test
go test -count=1 -p 1 ./...   # with POSTGRES_TEST_DATABASE_URL set
```

Be precise about what skipped. Several suites skip silently when Postgres or Redis is
missing, and "tests pass" has repeatedly meant "the interesting ones never ran". If you ran
a `scripts/*.py` harness, say which and give the count, e.g. `80/80`.

- [ ] I ran the suite **with** the services the change depends on
- [ ] I added a test in the matching package
- [ ] I updated `docs/openapi.yaml` if any route or payload changed
- [ ] I added a `CHANGELOG.md` line under Unreleased if the change is user-visible
- [ ] No secrets, tokens or `st=` ticket values appear in anything pasted above

## Notes for the reviewer

Anything you would want pointed out before you had to find it yourself: a deliberate
limitation, a case the tests do not cover, a follow-up you did not do here.
