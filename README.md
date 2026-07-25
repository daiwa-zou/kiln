# kiln

*Raw material fired into something durable, and fired again as it changes.*

A self-hosted platform that builds and maintains knowledge bases from your sources — code repos,
documents, and web pages — and keeps them current as those sources change.

Instead of retrieving from raw material on every question, kiln **compiles a persistent wiki** and
maintains it incrementally. When a source changes, only the affected pages are regenerated. When
nothing changes, a run costs nothing.

## Status

Early development. See the milestone plan for scope.

## How it works

```
sync → extract → map → plan → generate → validate → import → post-pass
```

- **Deterministic in Go**: source mapping, change detection, the page index, the log, the overview,
  and the content-hash cache that gates regeneration.
- **Agentic where it pays off**: page prose is written by a sandboxed `claude -p` pass that reads the
  real sources rather than pre-chunked text.

The LLM never writes the index, overview, or log — those are rebuilt from page frontmatter on every
run, so navigation can't drift.

## Architecture

Modular monolith plus a worker pool. One Go binary with two roles (`kiln serve`, `kiln worker`)
against Postgres, and a Next.js frontend in its own container. Scale by adding workers.

## Development

Requires Go 1.24, Postgres 16, and the `claude` CLI on `PATH`.

```bash
make test              # hermetic: unit + golden tests, no network, no database
make test-integration  # starts Postgres in Docker, runs everything including schema tests
make build             # -> bin/kiln
make migrate           # apply schema (advisory-lock guarded, safe to run concurrently)
make dev               # server + worker in one process
make db-up / db-down   # manage the test Postgres container
```

Integration tests key off `KILN_TEST_DATABASE_URL` and skip themselves when it is
unset, so `make test` stays fast and offline.

## License

TBD
