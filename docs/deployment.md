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

The recommended layout around these roles, for one VM and for a scalable
cloud deployment, plus the interim settings needed until the remaining
scaling work lands: [ROADMAP.md](../ROADMAP.md#target-deployment-architecture).

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
| `KILN_AUTH_DEFAULT_ROLE` | no | `member` (default) or `viewer`. The role `kiln admin token create --org` gives a membership it creates; set it where that command runs. Existing memberships keep their role, and `--admin` tokens bypass it. |
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

Neither is meant for a laptop: the chart assumes a managed Postgres and an
S3-compatible bucket, and its `fs` storage backend is an `emptyDir` that does
not survive a pod. For a persistent single-machine instance on Docker Desktop's
Kubernetes there is `make k8s-up`, described in
[`deploy/local-k8s/`](../deploy/local-k8s). It runs Postgres and the blob store
on PersistentVolumeClaims, pins everything to one node, and generates through
the Claude Code CLI rather than the Messages API. It is a development stack, not
a small production one.

## Observability

Prometheus metrics are served on a separate port (`127.0.0.1:9090` by default; set `metrics_addr` to `:9090` so Prometheus can reach it from another pod or host, as the Helm chart does) by both
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

## Sandboxing the CLI runner

This applies to `agent.runner = "cli"`, which shells out to `claude -p`. The
default `api` runner gives the model no tools and no filesystem, and none of
this is needed for it.

The CLI is the one component that executes model-chosen actions on untrusted
content: repository files, extracted documents, fetched pages, and the steering
and corrections any workspace member can write. A prompt injected through any
of them can steer it. kiln limits what it can do in three layers, each of which
assumes the one before it failed.

1. **The CLI's own sandbox.** Every step runs with `--restricted`, which removes
   the tools that run commands and confines Read, Grep and Glob to the working
   directory and the directories explicitly granted. The tool lists are
   explicit, with Bash, WebFetch, WebSearch and the rest denied by name. Needs
   CLI **2.1.248 or later**; an older CLI rejects the flag and every unit
   fails, rather than running unconfined.
2. **Writes confined to a scratch directory.** The agent's working directory is
   the unit's scratch directory, and the sources are granted read-only. Generate
   also carries an explicit `Edit(//<sources>/**)` deny rule, which the CLI
   checks before the permission mode, so `acceptEdits` cannot approve a write to
   them. For a local-path connector the sources are the operator's real
   directory, not a copy.
3. **A check afterwards.** Before the agent runs, kiln snapshots the unit's part
   of the source tree (path, size, mtime, mode); after each call it looks again.
   If anything differs the unit fails with `the agent modified the source tree`,
   none of its pages are imported, the paths are logged at error level, and
   nothing is restored. The snapshot is bounded (the root's own entries, the
   unit's inputs and the directories above them, and the unit's own directory,
   at most 20,000 entries), so it costs milliseconds per look but does **not**
   see a write to an unrelated file elsewhere in a large tree. A person editing
   the checkout during a build also trips it; the unit is rebuilt next run.

### What this does not do

These are in-process controls. The CLI runs as the same user as the worker, in
the same filesystem and PID namespace, and enforcement is the CLI's own
permission engine. kiln does not isolate the child at the operating-system
level yet, so what remains is:

- A bug in the CLI's path handling or permission checks is not contained.
  Against the same uid, an unconfined file tool could read
  `/proc/<worker pid>/environ` (which holds `KILN_MASTER_KEY`), the `fs` blob
  root (other tenants' uploads), other tenants' staging directories, and `HOME`
  with the CLI's credentials. The child's own environment is already minimal
  (`KILN_*` secrets are never passed to it); the worker's is not.
- The guard in layer 3 only sees what it samples.
- Nothing stops the agent writing misleading prose. Validation constrains where
  pages go and what they link to, not whether they are true.

### Recommended deployment

Treat the CLI child as a hostile process and give it only what a unit needs:

- Run it under **a separate uid** from the worker, or in **its own container**
  per call.
- Mount **only the unit's root (read-only) and its scratch directory (writable)**,
  at the same absolute paths kiln passes in argv, plus a private writable
  `HOME` for the CLI's session files and credentials.
- Do not mount the blob root, the worker's `/proc` (use a separate PID
  namespace, or `hidepid=2`), the worker's environment, or other workspaces'
  staging directories.
- Allow network egress only to the Anthropic API (or the gateway named by
  `agent.base_url`).
- Read-only root filesystem, no capabilities, no privilege escalation.

`agent.binary` can point at a wrapper that does this, since kiln only needs
something that accepts the CLI's arguments and writes its result to stdout. A
sketch, untested and not shipped, using Docker; the paths in argv are absolute
and must resolve identically inside:

```sh
#!/bin/sh
# /usr/local/bin/claude-sandboxed  (agent.binary)
# kiln runs this with the unit's scratch directory as the working directory.
# Every directory named by --add-dir is a source root and is mounted read-only.
set -eu
mounts=""
in_dirs=0
for a in "$@"; do
  case "$a" in
    --add-dir) in_dirs=1 ;;
    --*) in_dirs=0 ;;
    *) if [ "$in_dirs" = 1 ] && [ -d "$a" ]; then mounts="$mounts -v $a:$a:ro"; fi ;;
  esac
done
exec docker run --rm -i --network=claude-egress --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges --user 65533 \
  -e ANTHROPIC_API_KEY -e ANTHROPIC_BASE_URL -e HOME=/home/agent \
  --tmpfs /home/agent:rw,mode=1777 \
  -v "$PWD:$PWD" -w "$PWD" $mounts \
  claude-cli-image claude "$@"
```

The credential has to arrive through the environment or a mounted secret, since
the `HOME` above is thrown away. On Kubernetes the equivalent is a Job or
sidecar per unit with the same mounts, a `NetworkPolicy` allowing only the API,
and `readOnlyRootFilesystem`.

Sessions are stored by the CLI under its `HOME`, in a directory per working
directory. Each unit's scratch directory is new, so `~/.claude/projects`
accumulates one directory per unit per run; prune old ones on a schedule, or
point `HOME` at a volume that is discarded.

## Image

`ghcr.io/daiwa-zou/kiln` — multi-arch (amd64, arm64), non-root (uid 65532),
signed with cosign, published with SBOM and provenance attestations.

Tags: `vX.Y.Z` and `vX.Y` for releases, `edge` for the tip of main, and
`sha-<commit>` for exact pinning. Pin a digest in production.

The runtime carries `git`, `pdftotext` (poppler), and `pandoc`, because the
worker shells out to all three when syncing repositories and extracting
documents. A distroless image would build fine and fail at the first PDF.
