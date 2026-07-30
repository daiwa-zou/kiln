# Deploying kiln

kiln is one static Go binary plus Postgres and an S3-compatible bucket. There
is no broker, no sidecar, and no separate frontend to serve — the reading UI is
embedded in the binary. That is what makes it portable: anything that runs a
container and can reach Postgres and object storage can run kiln, whether that
is AWS, GCP, Azure, a bare-metal Kubernetes cluster, or a single VM.

## The topology

Three roles, one image:

| Role | Command | Scales with | State |
| --- | --- | --- | --- |
| API + reading UI | `kiln serve` | readers and API traffic | stateless |
| Build worker | `kiln worker` | build throughput | stateless |
| Migrations | `kiln admin migrate` | run once per deploy | — |

The queue is the `runs` table itself, claimed with `FOR UPDATE SKIP LOCKED`.
Adding build capacity means adding worker processes; nothing else changes, and
no coordination service is involved. Each worker claims **one run at a time** on
purpose: builds are LLM-bound, so per-process concurrency multiplies spend
rather than throughput.

`kiln serve --with-worker` runs both roles in one process. That is right for a
laptop or a single-node install and wrong for anything you intend to scale,
because it couples the two lifecycles: restarting the API to deploy a UI change
would interrupt an in-flight build.

## Requirements

- **Postgres 16+**, reachable from the API and every worker.
- **S3-compatible object storage** for uploaded documents (AWS S3, GCS in
  interop mode, Azure Blob via an S3 gateway, MinIO, R2, Backblaze). The `fs`
  backend writes to a local path instead, which is correct for a single node
  and wrong the moment workers span hosts — a worker cannot stage a document it
  cannot see.
- **An Anthropic API key**, unless every bench runs `agent.runner = "fake"`.

## Configuration

Every setting binds to an environment variable: the config key, `KILN_`
prefixed, dots as underscores. `agent.model` is `KILN_AGENT_MODEL`,
`database.max_conns` is `KILN_DATABASE_MAX_CONNS`. An empty variable is ignored
rather than applied, so leaving one unset in a template keeps kiln's default.

Secrets additionally accept a `_FILE` suffix pointing at a mounted file —
`KILN_MASTER_KEY_FILE=/run/secrets/master-key` — which is how Docker secrets and
Kubernetes secret volumes should deliver them. Prefer that to inline values:
an environment variable is visible to anything that can read `/proc`, and to
most `kubectl describe` output.

| Variable | Required | Notes |
| --- | --- | --- |
| `KILN_DATABASE_URL` | yes | Or the discrete `KILN_DATABASE_HOST`/`_PORT`/`_NAME`/`_USER`/`_PASSWORD`. |
| `KILN_MASTER_KEY` | yes | 32 random bytes (`openssl rand -hex 32`). Seals connector credentials. Rotate with `kiln admin rotate-key`. |
| `KILN_ANTHROPIC_API_KEY` | yes | Spent by generation. |
| `KILN_AUTH_MODE` | yes | `token` in any deployment. `none` makes every caller an admin and kiln refuses it on a non-loopback bind. |
| `KILN_STORAGE_BACKEND` | yes | `s3` or `fs`. |
| `KILN_STORAGE_BUCKET` / `_ENDPOINT` / `_ACCESS_KEY` / `_SECRET_KEY` | for s3 | Omit the keys to use the instance role / IRSA / workload identity chain. |
| `KILN_PUBLIC_URL` | recommended | External URL, for OAuth callbacks and links. |
| `KILN_AGENT_RUN_BUDGET_USD` | recommended | Per-run spend ceiling. |
| `KILN_WORKER_PERMITTED_SOURCE_ROOTS` | if using path sources | Allowlist for local-path connectors. Empty denies every local path. |

Run `kiln admin doctor` to validate configuration, database reachability, and
schema version in one pass.

## Docker Compose

The fastest complete deployment. It runs Postgres, MinIO, the API, and a
scalable worker pool:

```bash
cp .env.example .env          # set KILN_MASTER_KEY and KILN_ANTHROPIC_API_KEY
docker compose up -d
docker compose exec api kiln admin token create --login you --scopes read,write,admin
open http://localhost:8080    # paste the token when the UI asks
```

Add build capacity:

```bash
docker compose up -d --scale worker=4
```

The stack authenticates by default. Inside a container the server must bind
every interface to be reachable at all, and kiln refuses a wide-open bind with
auth disabled — there the container boundary, not the listen address, is what
limits reach, and the process cannot tell a published port from a private one.

## Kubernetes

See [`deploy/kubernetes/`](../deploy/kubernetes) for manifests and
[`deploy/helm/kiln/`](../deploy/helm/kiln) for a chart. The shape:

- A `Deployment` for the API behind a `Service` and `Ingress`, with `/readyz`
  as the readiness probe and `/healthz` as liveness. They differ deliberately:
  readiness reports database reachability, while liveness answers even when the
  database is down, so an outage in Postgres does not make the orchestrator
  kill every API pod.
- A separate `Deployment` for workers, with an `HPA`. Workers have no probes
  worth failing on — they hold no listener — so scale them on queue depth or
  CPU rather than health.
- A `Job` running `kiln admin migrate` as a Helm pre-upgrade hook. The advisory
  lock makes concurrent runs safe; API and workers refuse to start against an
  unexpected schema version, so this must succeed first.
- `terminationGracePeriodSeconds` on workers set above a typical build. SIGTERM
  begins a drain: the worker stops claiming immediately, and the in-flight run
  gets its grace window before being requeued. Cutting that short turns
  paid-for work into a requeue.

## Observability

Prometheus metrics are served on a separate port (`:9090` by default) by both
the API and every worker: request traffic, queue depth, build outcomes, and
model spend. See [observability.md](observability.md) for the metric reference,
alert rules, and scraping queue depth to autoscale workers.

## Operating

**Upgrades.** Roll the migration Job, then the API, then the workers. Workers
drain rather than abort, so a rolling update costs nothing in flight.

**Backups.** Postgres holds every page, run, and credential; object storage
holds uploaded documents. Back up both. A wiki can be rebuilt from its sources,
but the review queue, corrections, and steering documents are original human
input and exist nowhere else.

**Secrets.** `KILN_MASTER_KEY` is the one that cannot be regenerated: losing it
makes stored connector credentials unrecoverable. Rotation is online —
`kiln admin rotate-key` re-seals every credential in one transaction.

**Cost.** Set `agent.run_budget_usd` and a per-workspace `budget_usd`. Spend is
ledgered per run and per unit; the rolling budget window refuses new runs and
files a review item rather than failing silently.

## Image

`ghcr.io/daiwa-zou/kiln` — multi-arch (amd64, arm64), non-root (uid 65532),
signed with cosign, published with SBOM and provenance attestations.

Tags: `vX.Y.Z` and `vX.Y` for releases, `edge` for the tip of main, and
`sha-<commit>` for exact pinning. Pin a digest in production.

The runtime carries `git`, `pdftotext` (poppler), and `pandoc`, because the
worker shells out to all three when syncing repositories and extracting
documents. A distroless image would build fine and fail at the first PDF.
