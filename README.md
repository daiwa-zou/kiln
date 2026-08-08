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

## What it reads, and what you get

A **bench** is one wiki with its own sources. Three kinds feed it, and they merge
into a single wiki whose pages can link across the boundary:

| Source | What it ingests |
| --- | --- |
| **Repositories** | A local path or an https remote, cloned shallow. Partitioned into modules, with a dependency graph extracted deterministically. |
| **Documents** | Uploaded or from a folder — `.md`, `.pdf`, `.docx`, `.pptx`, `.xlsx`, `.epub`, `.html` and more. Long documents split into chapters that regenerate independently. |
| **Web pages** | Fetched URLs, extracted to text. |

Out comes an interlinked markdown wiki: six page types (`entity`, `concept`,
`source`, `query`, `comparison`, `synthesis`) plus three documents kiln maintains
itself — an index, an overview, and a build log.

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

**Only changed work costs anything.** A per-unit content hash gates regeneration,
so a run over unchanged sources makes no model calls at all. On webhook builds,
commit-range routing narrows the plan further, and cosmetic files (images,
lockfiles, editor chrome) never trigger a rebuild on their own. Spend is bounded
at six independent layers, from that hash gate down to a per-run ledger that
reserves before it spends and a rolling per-bench budget window.

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

A full walkthrough — topology, the build pipeline stage by stage, how material
flows through it, where cost is bounded, the data model, and every configurable —
is in [docs/architecture.md](docs/architecture.md). The short version:

Modular monolith: one Go binary, four roles, against Postgres.

| Command | Role |
| --- | --- |
| `kiln serve` | HTTP API and the embedded reading UI. `--with-worker` also builds, which is the right shape for one node. |
| `kiln worker` | Claims queued runs and builds them. Scale by adding processes. |
| `kiln build` | Runs the pipeline against a local directory — no server, no database. |
| `kiln mcp` | Serves a bench to agents over MCP. Reads through the API. |
| `kiln admin` | Tokens, migrations, key rotation, `doctor`. |

Builds scale by adding worker processes — the queue is the runs table itself,
claimed with `FOR UPDATE SKIP LOCKED`, one active run per bench. There is no
external broker.

The API requires a bearer token by default — mint one with `kiln admin token create`.
Uploaded documents land in object storage (`storage.*`: S3/MinIO, or a mounted
volume via the fs backend); document uploads accept up to 32 MiB per file, so a
reverse proxy in front of kiln needs its body limit raised to match (nginx:
`client_max_body_size 34m`).

Planned, not yet built: a Next.js frontend in its own container.

## Agents can read it

`kiln mcp` serves a bench to any MCP-capable agent over stdio, so an agent
answers from the compiled wiki instead of re-reading your sources every time.

```json
{
  "mcpServers": {
    "kiln": {
      "command": "kiln",
      "args": ["mcp", "--url", "http://127.0.0.1:8080", "--workspace", "my-bench"],
      "env": {"KILN_TOKEN": "..."}
    }
  }
}
```

Seven tools: `search_wiki` and `read_page` carry most traffic, with
`wiki_overview` for orientation, `list_benches` and `list_pages` for
enumeration, `page_backlinks` for context, and `wiki_gaps` — which is what
lets an agent tell *"the wiki says nothing about X"* from *"the wiki has not
covered X yet"*.

It reads over the HTTP API rather than the database, so it needs no Postgres
credentials and works against an instance running anywhere; the token decides
which benches it can see. Omit `--workspace` and every tool takes a `bench`
argument instead.

## Configuring it

Every setting lives in a TOML file or an environment variable: the config key,
`KILN_` prefixed, dots as underscores — `agent.model` is `KILN_AGENT_MODEL`. An
empty variable is ignored rather than applied, so an unset one keeps kiln's
default. Secrets also take a `_FILE` suffix pointing at a mounted file, which is
how Docker and Kubernetes secrets should deliver them.

The knobs worth knowing before anything else:

| Setting | Default | Why you'd touch it |
| --- | --- | --- |
| `agent.model` | `claude-sonnet-5` | The model that writes pages. |
| `agent.run_budget_usd` | `6.00` | Hard ceiling per run, at any concurrency. |
| `agent.max_pages_per_run` | `12` | Caps one run; the remainder is deferred to a follow-up. |
| `agent.unit_concurrency` | `1` | Raise it to build a large bench in minutes rather than hours. |
| `agent.runner` | `api` | Names a registered model provider; `fake` runs the whole pipeline with zero spend. Adding one is a registration, not a fork — see [docs/architecture.md](docs/architecture.md). |
| `auth.mode` | `token` | `none` is refused on any non-loopback bind. |
| `worker.permitted_source_roots` | *(empty)* | Allowlist for local-path sources. Empty denies every one. |

Every key, with defaults, is in
[docs/architecture.md](docs/architecture.md#configuration-reference).
`kiln admin doctor` checks configuration, database reachability, and schema
version in one pass.

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
are in [docs/observability.md](docs/observability.md). The reading UI targets
WCAG 2.2 AA; what that means here and what was measured is in
[docs/accessibility.md](docs/accessibility.md).

## Development

Requires Go 1.25 and Postgres 16. The `claude` CLI is only needed when
`agent.runner = "cli"`; the default runner calls the Anthropic API directly.

`make` with no arguments (or `make help`) prints every target grouped by where
it runs, which is the fastest way to find one. The full reference follows.

Three targets spend real money — `make dev-cli`, `make k8s-up`, and
`make test-claude`. Everything else is free.

### Build and check

| target | what it does |
| --- | --- |
| `make build` | compiles `bin/kiln`, stamping the version from `git describe`. |
| `make all` | `fmt`, `test`, `build`. |
| `make test` | unit and golden tests. Hermetic: no network, no database, no cost. |
| `make test-verbose` | the same suite with `-v -race`. |
| `make test-integration` | starts Postgres in Docker and runs everything, schema tests included. |
| `make cover` | coverage profile, prints the total, regenerates the README badge. This is the CI gate — a plain `go test` passes while coverage is under the floor. |
| `make lint` | golangci-lint, pinned to the version CI runs. |
| `make vulncheck` | govulncheck against the Go vulnerability database. |
| `make fmt` / `make tidy` | gofmt every package; prune and sync `go.mod`. |
| `make db-up` / `make db-down` | start or remove the Postgres container on `:55432`, shared by the tests and the dev database. |
| `make test-claude` | **spends money.** The one check the suite cannot make: a real `claude`, a real model call, real prose imported as a page. Needs a logged-in session; costs a few cents. |
| `make clean` | remove `bin/` and `dist/`. |

Integration tests key off `KILN_TEST_DATABASE_URL` and skip themselves when it
is unset, so `make test` stays fast and offline.

### Running it locally

| target | what it does |
| --- | --- |
| `make dev-up` | **start here.** Database, schema, a built wiki, then the server — from nothing, in one command. |
| `make dev` | just the server and worker, against a wiki that already exists. Fake runner: full pipeline, placeholder prose, zero cost. |
| `make dev-cli` | **spends money.** `make dev` with the real Claude Code CLI instead of the fake runner, a master key so credential-backed connectors work, and budgets sized for real documents. Override with `ANALYZE_USD=`, `PAGE_USD=`, `RUN_USD=`; set all three to `0` to remove the caps entirely. |
| `make dev-build` | one build of this repository into the dev wiki, from another terminal. |
| `make dev-seed` | builds this repository into the dev wiki until it converges. A run stops at `max_pages_per_run` and defers the rest, so this loops until a run reports nothing changed. |
| `make dev-db` / `make migrate-dev` | create the `kiln_dev` database; apply migrations to it. |
| `make dev-clean` | drop `kiln_dev` and the local blob store. |
| `make migrate` | apply migrations using the default config, for a real deployment on this machine. Advisory-lock guarded, so concurrent runs are safe. |
| `make doctor` | configuration, database, schema, master key, and — when the CLI runner is selected — whether the claude session is usable. Free. `PROBE=1` adds a real generation round trip, the only conclusive answer about a credential. |
| `make token` | mint an API token against the dev database. `LOGIN=you` sets the login. |
| `make status` | is the local stack up: the Postgres container and the server's readiness endpoint. |

### The same questions, in Kubernetes

The operational commands exist for both places kiln runs, under matching names.
The in-cluster ones run inside a pod, because that is where the configuration
and the database are.

| | local | Kubernetes |
| --- | --- | --- |
| is it healthy | `make doctor` | `make k8s-doctor` |
| apply migrations | `make migrate` / `make migrate-dev` | `make k8s-migrate` |
| mint an API token | `make token` | `make k8s-token` |
| what is running | `make status` | `make k8s-status` |

| target | what it does |
| --- | --- |
| `make k8s-up` | **spends money.** Build, load, apply, wait; prints the URL and a token. Generation runs through the Claude Code CLI, so every build has a real cost. |
| `make k8s-down` | stop the workloads and keep the volumes — the wiki is still there next time. |
| `make k8s-purge` | delete the namespace and every volume in it, data included. |
| `make k8s-status` | pods, services, and volumes. |
| `make k8s-logs` | follow the worker, where generation happens. `C=api` or `C=postgres` for the others. |
| `make k8s-doctor` | the in-cluster `make doctor`, run in the worker pod — the one whose CLI credential state matters. `PROBE=1` applies here too. |
| `make k8s-migrate` | apply migrations to a cluster already running, without a redeploy. `k8s-up` runs them as a Job on the way in. |
| `make k8s-token` | mint another API token. |
| `make k8s-sync` | `SRC=/path/to/repo` copies a local directory into the sources volume. The cluster's nodes cannot see your filesystem, so copying is the way in. |
| `make k8s-reauth` | discard the credential the worker refreshed for itself and re-seed from the secret, after a re-exported Claude Code session. |
| `make k8s-shell` | a shell in the worker pod. |

### Images and charts

| target | what it does |
| --- | --- |
| `make image` | build the container image CI publishes. `VERSION=` overrides the tag. |
| `make compose-up` / `make compose-down` | bring the docker compose stack up, or down with its volumes. |
| `make helm-lint` | lint the Helm chart. |
| `make manifests` | render the chart to plain YAML for GitOps repositories and Kustomize overlays. |

### Local end-to-end run (no API key, zero cost)

One command, from nothing to a wiki you can read:

```bash
make dev-up       # then open http://127.0.0.1:8080
```

Docker is the only prerequisite — `make dev-up` starts Postgres itself. It runs
against [config.dev.toml](config.dev.toml) and does four things: starts the
Postgres container (the same one the tests use, but a dedicated `kiln_dev`
database that test runs cannot wipe), applies migrations, builds this repository
into the dev wiki until it converges, and starts `serve --with-worker` on
:8080.

Every component a deployment has is present. Object storage is the one
substitution: `storage.backend = "fs"` writes blobs under `.dev/`, so no MinIO
is needed. The agent is `agent.runner = "fake"` — a deterministic runner that
exercises every real pipeline stage (sync, map, plan, validate, import, the
queue, the budget ledger, the UI) while generating placeholder prose. **No API
key, no spend.**

```bash
make dev          # just the server, against a wiki that already exists
make dev-build    # one build into the dev wiki, from another terminal
make dev-clean    # drop the dev database and blobs
```

A build stops at `agent.max_pages_per_run` and defers the rest, so this
repository takes two runs to cover and a third to report *"nothing changed; no
model calls, no cost"*. `make dev-up` runs that loop for you; `make dev-build`
is a single run.

To exercise the queue path, open the **Sources** view at http://localhost:8080,
add the repo as a source (path `$PWD`), and hit Build now — or do the same over
the API:

```bash
curl -X POST localhost:8080/api/v1/workspaces/kiln/connectors \
  -H 'Content-Type: application/json' \
  -d '{"kind":"git","name":"local","config":{"path":"'$PWD'"}}'
curl -X POST localhost:8080/api/v1/workspaces/kiln/runs
```

then watch it on the Runs view, which reports the plan as it executes — how many
units are done, which one is being written now, and what is still queued behind
it. `KILN_AGENT_FAKE_LATENCY=2s` slows calls enough to watch that happen. The Sources view also takes document uploads
(drag-and-drop; stored via `storage.*`, fs-backed in dev) and web page sources,
and a bench fed only by documents or web pages builds without any repository. The fake runner
charges a synthetic $0.01/call so cost columns, estimates, and budget windows
behave realistically; `KILN_AGENT_FAKE_FAIL_UNITS=module:foo` injects failures
for exercising partial runs, and `KILN_AGENT_FAKE_LATENCY=2s` slows calls down
enough to watch state transitions.

`make lint` needs golangci-lint **v2** (`brew install golangci-lint`); the v1 series
cannot read `.golangci.yml`.

### Local Kubernetes, generating through the Claude Code CLI

`make dev-up` proves the pipeline; it does not prove generation, because the
fake runner writes the prose. `make dev-cli` is the shortest way to prove that
too — the same local stack with the real CLI runner, using the `claude` session
already on your machine.

What Kubernetes adds is persistence and the shape of a deployment: a worker in
its own pod, Postgres and the blob store on volumes, and generation running
against sources staged inside the cluster rather than read from your working
copy. `agent.runner = "cli"` there as well — the worker shells out to
`claude -p` against the staged sources:

```bash
ANTHROPIC_API_KEY=sk-ant-... make k8s-up       # then open http://localhost:8080
```

Postgres, the blob store, and a staging area for local sources are all on
PersistentVolumeClaims, so `make k8s-down` stops the workloads without losing
the wiki and `make k8s-up` picks it back up. `make k8s-purge` is the destructive
one. Unlike `make dev-up`, **this spends real money on every build**; the run
budget and page cap default low for that reason.

A local repository is copied in rather than mounted — the cluster's nodes are
containers that cannot see your filesystem:

```bash
make k8s-sync SRC=~/code/my-project     # ingest it as /sources/my-project
```

What it creates, how it is configured, and why it is shaped the way it is:
[deploy/local-k8s/README.md](deploy/local-k8s/README.md). It is a development
stack, not a small production one — for that, use the Helm chart.

## Documentation

| Document | What it covers |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | How the whole system works: topology, the pipeline stage by stage, data flow, cost control, the data model, the MCP server, failure modes, and every configurable. Start here. |
| [docs/deployment.md](docs/deployment.md) | Running it: requirements, Compose and Kubernetes, upgrades, backups, operating notes. |
| [docs/storage-and-retrieval.md](docs/storage-and-retrieval.md) | Why Postgres stays the primary store, where full-text search falls down for agent queries, and what to do about it. |
| [docs/observability.md](docs/observability.md) | Metrics, alerts, and the health endpoints. |
| [docs/accessibility.md](docs/accessibility.md) | What WCAG 2.2 AA means for the reading UI, and what was measured. |
| [ROADMAP.md](ROADMAP.md) | What each shipped milestone contained. |

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
