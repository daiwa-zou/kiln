<p align="center"><img src="docs/logo.svg" alt="kiln" width="220"></p>

# kiln

[![ci](https://github.com/daiwa-zou/kiln/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/daiwa-zou/kiln/actions/workflows/ci.yml)
[![security](https://github.com/daiwa-zou/kiln/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/daiwa-zou/kiln/actions/workflows/security.yml)
[![release](https://github.com/daiwa-zou/kiln/actions/workflows/release.yml/badge.svg)](https://github.com/daiwa-zou/kiln/releases)
[![coverage](./.github/badges/coverage.svg)](https://github.com/daiwa-zou/kiln/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)

*Raw material fired into something durable, and fired again as it changes.*

A self-hosted platform that builds and maintains knowledge bases from your sources — code repos,
documents, and web pages — and keeps them current as those sources change.

Instead of retrieving from raw material on every question, kiln **compiles a persistent wiki** and
maintains it incrementally. A per-unit content-hash gate means only units whose sources
actually changed spend anything on regeneration, and a run over unchanged sources costs
nothing. Commit-range change routing narrows the work further on webhook-triggered builds.

## Status

All four roadmap milestones have shipped — the human loop and reader (M1), server-side builds
(M2), the multi-tenant SaaS shell with GitHub sign-in and webhooks (M3), and the web connector,
graph view, and cost attribution (M4). See [ROADMAP.md](ROADMAP.md) for what each contains.

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

## The human loop

Pages are never hand-edited — regeneration would clobber the edit — so human judgment enters
through three channels, all editable from the UI (or the write API):

- **Steering docs** (`purpose`, `schema`): injected into every prompt; the main lever for changing
  a wiki's character without touching code.
- **Page corrections**: pinned beside a page and re-injected into every future rebuild of it, so
  what you teach the wiki survives regeneration.
- **The review queue**: the wiki's questions for its humans. The agent files contradictions,
  uncertainties, and gaps it would otherwise guess at; a disappeared source files a deletion
  request. Nothing is ever deleted until someone approves it there.

## Architecture

Modular monolith: one Go binary against Postgres. `kiln serve` runs the HTTP API and the
embedded reading UI; `kiln build` runs the generation pipeline against a local directory;
`kiln worker` claims queued runs from the database and builds them through the same pipeline
(`kiln serve --with-worker` runs both in one process for single-node deployments). Builds
scale by adding worker processes — the queue is the runs table itself, claimed with
`FOR UPDATE SKIP LOCKED`, one active run per bench.
The API requires a bearer token by default — mint one with `kiln admin token create`.
Uploaded documents land in object storage (`storage.*`: S3/MinIO, or a mounted
volume via the fs backend); document uploads accept up to 32 MiB per file, so a
reverse proxy in front of kiln needs its body limit raised to match (nginx:
`client_max_body_size 34m`).

Planned, not yet built: a Next.js frontend in its own container.

## Deploying

One image runs every role; the reading UI is embedded in the binary, so there
is no separate frontend to serve. Anything that can run a container and reach
Postgres and an S3-compatible bucket can run kiln — AWS, GCP, Azure, bare-metal
Kubernetes, or a single VM.

```bash
cp .env.example .env          # set KILN_MASTER_KEY and KILN_ANTHROPIC_API_KEY
docker compose up -d
docker compose exec api kiln admin token create --login you --scopes read,write,admin --admin
open http://localhost:8080
```

Builds scale by adding workers, independently of the API:

```bash
docker compose up -d --scale worker=4
```

For Kubernetes there is a Helm chart at [deploy/helm/kiln](deploy/helm/kiln)
(separate API and worker Deployments, autoscaling, a migration hook, and a
restricted pod security context) and [deploy/kubernetes](deploy/kubernetes) for
plain manifests. Configuration, upgrades, backups, and operating notes are in
[docs/deployment.md](docs/deployment.md); metrics, alerts, and health endpoints
are in [docs/observability.md](docs/observability.md).

## Development

Requires Go 1.25 and Postgres 16. The `claude` CLI is only needed when
`agent.runner = "cli"`; the default runner calls the Anthropic API directly.

```bash
make test              # hermetic: unit + golden tests, no network, no database
make test-integration  # starts Postgres in Docker, runs everything including schema tests
make cover             # coverage profile + regenerate the README coverage badge
make lint              # golangci-lint, same config CI runs
make vulncheck         # govulncheck against the Go vulnerability database
make build             # -> bin/kiln
make migrate           # apply schema (advisory-lock guarded, safe to run concurrently)
make dev               # server + worker in one process
make db-up / db-down   # manage the test Postgres container
```

Integration tests key off `KILN_TEST_DATABASE_URL` and skip themselves when it is
unset, so `make test` stays fast and offline.

### Local end-to-end run (no API key, zero cost)

`make dev` brings up the whole system against [config.dev.toml](config.dev.toml):
Postgres (in the same container the tests use, but a dedicated `kiln_dev`
database that test runs cannot wipe), migrations, and `serve --with-worker`
with `agent.runner = "fake"` — a deterministic runner that exercises every real
pipeline stage (sync, map, plan, validate, import, the queue, the budget
ledger, the UI) while generating placeholder prose with zero API spend.

```bash
make dev          # terminal 1: API + UI + worker on :8080
make dev-build    # terminal 2: build kiln itself into the dev wiki
make dev-build    # again: "nothing changed; no model calls, no cost"
make dev-clean    # drop the dev database and blobs
```

To exercise the queue path, open the **Sources** view at http://localhost:8080,
add the repo as a source (path `$PWD`), and hit Build now — or do the same over
the API:

```bash
curl -X POST localhost:8080/api/v1/workspaces/kiln/connectors \
  -H 'Content-Type: application/json' \
  -d '{"kind":"git","name":"local","config":{"path":"'$PWD'"}}'
curl -X POST localhost:8080/api/v1/workspaces/kiln/runs
```

then watch it on the Runs view. The Sources view also takes document uploads
(drag-and-drop; stored via `storage.*`, fs-backed in dev) and web page sources,
and a bench fed only by documents or web pages builds without any repository. The fake runner
charges a synthetic $0.01/call so cost columns, estimates, and budget windows
behave realistically; `KILN_AGENT_FAKE_FAIL_UNITS=module:foo` injects failures
for exercising partial runs, and `KILN_AGENT_FAKE_LATENCY=2s` slows calls down
enough to watch state transitions.

`make lint` needs golangci-lint **v2** (`brew install golangci-lint`); the v1 series
cannot read `.golangci.yml`.

## CI

| Workflow | Trigger | Jobs |
| --- | --- | --- |
| `ci` | push to main, PR | lint (golangci-lint + `go mod tidy` drift), hermetic race tests with coverage, Postgres integration tests, build + `kiln version` smoke test |
| `security` | push, PR, weekly | govulncheck, gitleaks |
| `release` | `v*` tag | GoReleaser: linux/darwin/windows × amd64/arm64, checksums, changelog |

Because this repository is private, the badges above render only for signed-in users
with access to it — GitHub serves workflow badges against the viewer's own session and
will not expose them externally. Coverage is a committed SVG regenerated by CI on every
push to `main`, for the same reason: shields.io and Codecov cannot read a private repo.

CodeQL is deliberately absent — code scanning on private repositories requires the paid
GitHub Code Security add-on, so a CodeQL workflow here would fail every run. `gosec`
(via golangci-lint) and `govulncheck` cover that ground instead.

## License

TBD
