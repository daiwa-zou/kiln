# Deploying kiln

kiln is one static Go binary plus Postgres and an S3-compatible bucket. There
is no broker, no sidecar, and no separate frontend to serve — the reading UI is
embedded in the binary. That is what makes it portable: anything that runs a
container and can reach Postgres and object storage can run kiln, whether that
is AWS, GCP, Azure, a bare-metal Kubernetes cluster, or a single VM.

## The topology

Four roles, one image:

| Role | Command | Scales with | State |
| --- | --- | --- | --- |
| API + reading UI | `kiln serve` | readers and API traffic | stateless |
| Build worker | `kiln worker` | build throughput | stateless |
| Scheduler | `kiln scheduler` | nothing: exactly one | stateless |
| Migrations | `kiln admin migrate` | run once per deploy | — |

The queue is the `runs` table itself, claimed with `FOR UPDATE SKIP LOCKED`.
Adding build capacity means adding worker processes; nothing else changes, and
no coordination service is involved. Each worker claims **one run at a time** on
purpose: builds are LLM-bound, so per-process concurrency multiplies spend
rather than throughput.

The scheduler runs the periodic duties once for the deployment: requeueing
runs whose worker died, enqueueing poll-triggered sources, the hourly GC
sweep, and the queue-depth gauge. Workers run those duties themselves unless
told a scheduler does (see [The scheduler](#the-scheduler)).

`kiln serve --with-worker` runs the API, a worker and the scheduler duties in
one process. That is right for a
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
| `KILN_TRUSTED_PROXIES` | behind a proxy | Comma-separated CIDRs or bare IPs of your reverse proxies. See [Behind a reverse proxy](#behind-a-reverse-proxy). |
| `KILN_AGENT_RUN_BUDGET_USD` | recommended | Per-run spend ceiling. |
| `KILN_WORKER_PERMITTED_SOURCE_ROOTS` | if using path sources | Allowlist for local-path connectors. Empty denies every local path. |
| `KILN_WORKER_SCHEDULER_DUTIES` | with a scheduler | `true` (default): the worker runs the requeue, polling, sweeps and queue gauge itself. Set `false` on workers once a `kiln scheduler` runs; see [The scheduler](#the-scheduler). |
| `KILN_DATABASE_QUERY_EXEC_MODE` | behind a pooler | `cache_describe` behind PgBouncer or RDS Proxy in transaction mode. Empty keeps pgx's default. See [Connection pooling](#connection-pooling). |

Run `kiln admin doctor` to validate configuration, database reachability, and
schema version in one pass.

### Behind a reverse proxy

Behind an ingress or load balancer every request arrives from the proxy's
address. Unauthenticated callers (write routes with `auth.mode = none`, and
webhook deliveries that fail verification) are rate-limited by address, so by
default they all share the proxy's bucket. Set `trusted_proxies`
(`KILN_TRUSTED_PROXIES`, Helm `config.trustedProxies`) to fix that:

```sh
KILN_TRUSTED_PROXIES=10.0.0.0/16,192.0.2.10
```

When the TCP peer is in the list, kiln reads `X-Forwarded-For` and walks it
from the right, skipping trusted hops. The first untrusted address is the
client. A request from any other peer is keyed on that peer, and its headers are
ignored. That is what stops a caller from choosing its own bucket by writing the
header itself. At most 32 entries are examined. An entry that is not an address
stops the walk at the last trusted hop.

- **List only proxies that append the address they received from.** That is
  nginx's `$proxy_add_x_forwarded_for`, AWS ALB, GCP and Azure load balancers,
  Traefik in its default mode, and Envoy with `use_remote_address: true`
  (Envoy's default leaves the header untouched). A proxy that passes the client's
  header through unchanged, or replaces it with a value the client supplied,
  makes the header the client's to write.
- **List every hop.** With a CDN in front of an ingress, both belong in the
  list. Missing one makes that hop the "client", so every caller shares its
  bucket again. That fails safe, but it is no better than the default.
- **Keep the ranges tight.** A trusted range that also holds clients lets those
  clients set their own address. On Kubernetes, list the ingress controller's
  pod or Service IPs, not the cluster pod CIDR: every pod in the cluster sits
  in that CIDR. kiln warns at startup about IPv4 /8, IPv6 /32 or anything
  broader, and logs the effective list. The warning is a smell test, not a
  guarantee. A /16 that holds clients is just as unsafe and draws no warning.
- **The limiter has a degraded mode.** It tracks at most 10,000 addresses per
  hour. Past that, new addresses share 64 overflow buckets chosen by hash. A
  flood of fresh addresses then throttles other newcomers, never a caller who
  already holds a bucket.
- **IPv6 callers are bucketed per /64**, with or without proxies, because one
  host is routinely given a whole /64. IPv4-mapped addresses count as IPv4.
- `X-Real-IP` and `Forwarded` are not read. This setting does not change how
  `X-Forwarded-Proto` is handled. That header is consulted only when
  `public_url` is unset, and then from any peer. Behind a proxy, set
  `public_url`.

Authenticated callers are keyed on their user, so none of this affects them.

## Docker Compose

The fastest complete deployment. It runs Postgres behind PgBouncer, MinIO, the
API, a scalable worker pool, and one scheduler:

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

Never scale `scheduler`: one runs the duties for every worker.

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
- A single-replica `Deployment` for the scheduler (`scheduler.enabled`, on by
  default) with a `Recreate` rollout, readiness and liveness on its metrics
  port, and only the database and storage secrets. Enabling it sets
  `KILN_WORKER_SCHEDULER_DUTIES=false` on the workers in the same release.
- A `Job` running `kiln admin migrate` as a Helm pre-upgrade hook. Each
  migration runs in its own transaction under a transaction-scoped advisory
  lock, so concurrent runs serialize, and a run behind a hung one gives up
  after five minutes with a diagnosis. API and workers refuse to start against
  an unexpected schema version, so this must succeed first. It always connects
  to Postgres directly, never through the pooler.
- Optionally (`pooler.enabled`), PgBouncer in front of Postgres for the API and
  workers; see [Connection pooling](#connection-pooling).
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

## The scheduler

`kiln scheduler` runs four duties, each on its own period and timeout so a
slow one (a blob sweep stuck on object storage) delays only itself:

| Duty | Period | What it does |
| --- | --- | --- |
| `requeue_stale` | 30s | Returns runs whose worker stopped heartbeating for `worker.stale_after` to the queue. |
| `poll_sources` | 1m | Enqueues poll-triggered connectors that are due (`worker.source_poll_interval`). |
| `sweep` | 1h | Enforces the retention windows, then drains the pending blob deletes. The first sweep waits one to five minutes after start, so a crash-looping pod does not sweep on every restart. |
| `queue_depth` | 10s | Publishes `kiln_queue_depth`. |

It reads `worker.stale_after`, `worker.source_poll_interval` and the
`storage.*` retentions, and needs the database and object storage. It never
reads the master key or a model key, and the chart and compose give it
neither. Without object storage it leaves the blob deletes queued and warns
with their count on every sweep. That includes the chart's `fs` backend, which
is single-pod only: the bytes sit on the API pod's disk, which neither the
scheduler nor a worker can reach.
`kiln scheduler --once` runs every duty once and exits.

Run one. Every duty tolerates running concurrently in another process (the
requeue skips locked rows, a poll enqueue merges onto the active run, sweeps
are deletes), so two schedulers, or workers still running duties beside one,
cost duplicate work and never corruption. There is no lease to hand over.

Workers run the duties themselves while `worker.scheduler_duties` is `true`,
the default. That is what makes upgrading safe in any order:

| Scenario | Result |
| --- | --- |
| New image, old chart or compose file | Unchanged: workers keep the duties, as before. |
| New chart (`scheduler.enabled`, the default) | The scheduler Deployment appears and workers get `KILN_WORKER_SCHEDULER_DUTIES=false` (`worker.schedulerDuties: auto`) in the same release. Workers give the duties up as soon as their new pods start, whether or not the scheduler ever comes up. With `helm upgrade --wait` a scheduler that never goes Ready fails the release; without it (Argo CD, Flux) the duties stop until someone notices, so upgrade in two steps below. |
| Scheduler cannot run (crash loop, no scheduling room) | The alerts in [observability.md](observability.md#scheduler-alerts) fire. Set `scheduler.enabled=false`, which hands the duties back to the workers. |
| Rolling back | No schema change is involved; an older image ignores the variable and runs the duties in every worker. |
| `deploy/local-k8s`, hand-written manifests | Unchanged until you add a scheduler: workers keep the duties. Add one, then set `KILN_WORKER_SCHEDULER_DUTIES=false` on the workers. |

**Two-step upgrade**, for any deployment that does not wait on readiness:

1. Upgrade with `worker.schedulerDuties: true`. The scheduler starts and the
   workers keep their duties beside it (duplicate work, never corruption).
2. Once `kubectl rollout status deploy/<release>-scheduler` reports it Ready,
   upgrade again with `worker.schedulerDuties: auto` (the default).

The one unsafe combination is workers with the variable `false` and no
scheduler: nothing requeues, polls or sweeps. `KilnSchedulerAbsent` exists for
exactly that, and each worker logs `scheduler_duties=false` at start.

`kiln serve --with-worker` always runs the duties, whatever the variable says:
it is the whole deployment.

## Connection pooling

Every kiln process holds its own connection pool, so connections grow with
replicas: the chart's autoscaling ceilings (10 API pods at 20, 20 workers at
6) would open 320 against an untuned Postgres limit of 100. A pooler in
transaction mode puts those clients on a small, fixed set of server
connections, because a pooled client holds a server connection only for the
length of a transaction.

kiln is correct behind one. Nothing it does outlives a transaction — no
session `SET`, `LISTEN`, temporary tables or session advisory locks (a test
scans the source for them) — and migrations take a transaction-scoped lock.
CI runs the integration suite through PgBouncer in both exec modes below
(`make test-pooled` locally).

**Compose** runs PgBouncer by default ([`deploy/pgbouncer/`](../deploy/pgbouncer)):
the API and workers connect to `pgbouncer:6432` with `cache_describe` and pools
of 8 and 4, and `migrate` connects to Postgres directly. The pooler's userlist
is written from `POSTGRES_PASSWORD` when the container starts.

**Helm**: set `pooler.enabled=true` with `config.database.host` pointing at
Postgres and a `database-password` key in the secret. The chart then deploys
PgBouncer (two replicas, a PodDisruptionBudget, a rolling update that never
drops below the replica count), points the API and workers at it with
`cache_describe` and pools of `pooler.apiMaxConns` (8) and
`pooler.workerMaxConns` (4), and leaves the migrate Job on
`config.database.host`: it runs as a pre-install hook, before the pooler
exists. While the pooler is enabled a `database-url` key in the secret is
ignored, because a URL would send every pod past the pooler, and an inline
`secrets.databaseURL` is refused outright. The chart cannot see inside an
`existingSecret` at render time: if it lacks `database-password`, the pooler
pods stop at `CreateContainerConfigError` naming the missing key.

The chart now always sets the API's pool size: `config.database.maxConns`
(20) without the pooler, `pooler.apiMaxConns` with it. Before this,
`config.database.maxConns` was never passed to the API, which ran the
binary's default of 20; if you had changed it, the API now uses your value.

Until M10 adds NetworkPolicies, the pooler Service is reachable from any pod
in the cluster, as Postgres usually is; it still requires the database
password.

**Another pooler** (RDS Proxy, a managed PgBouncer): leave `pooler.enabled`
off, point `config.database.host` at the pooler for the API and workers, and
set `KILN_DATABASE_QUERY_EXEC_MODE=cache_describe`. Run migrations against
Postgres itself.

### Exec mode

`database.query_exec_mode` decides how pgx sends queries, and so whether they
survive landing on a different server connection than the last one:

| Mode | Behind a transaction pooler | Notes |
| --- | --- | --- |
| empty / `cache_statement` | Only with prepared-statement tracking (PgBouncer 1.21+ `max_prepared_statements`, which the bundled config sets) | pgx's default: named statements, prepared once per connection. |
| `cache_describe` | Yes, with any pooler | Unnamed statements; parameter types still come from the server, so binary encoding is kept. Costs a server-side parse per query. The pooled default. |
| `exec` | Yes | Describes every query: an extra round trip each time. |

`describe_exec` and `simple_protocol` are refused at startup, from
`database.query_exec_mode` and from a `default_query_exec_mode` parameter in
`database.url` alike.

**Upgrading.** Two changes apply to every deployment, pooled or not:

- A database URL carrying `default_query_exec_mode=simple_protocol` or
  `=describe_exec` was honoured before and now fails at startup. Remove the
  parameter and set `database.query_exec_mode = "cache_describe"`
  (`KILN_DATABASE_QUERY_EXEC_MODE`) if the reason was a pooler, or leave it
  empty for pgx's default.
- Connections now expire up to 10 minutes after `database.max_conn_lifetime`
  (1h) rather than exactly at it, so a process's connections no longer all
  reconnect in the same instant.

### Sizing

Size the server side for concurrent **transactions**, not connected clients.
Proposed starting points, not measurements:

- **Clients.** 10 API pods × 8 + 20 workers × 4 + the scheduler's 2 = 162
  client connections, well under `max_client_conn` (400) on each PgBouncer pod — either pod must be able
  to take every client while the other restarts. `min_conns` stays at 2 per
  process; idle pooler clients are cheap.
- **Server connections.** Each PgBouncer pod keeps its own pool of up to
  `default_pool_size` + `reserve_pool_size` (25 + 5) server connections, so two
  pods can open 60. Keep `replicas × (defaultPoolSize + reservePoolSize)` at or
  below `max_connections` minus `superuser_reserved_connections` (3) minus
  about 10 for migrations, admin sessions and monitoring.
- **What holds one.** Every open transaction, for its whole length: a worker's
  import transaction, and a transaction *waiting* on the per-workspace import
  lock or the migration lock, holds a server connection while it waits. Pools
  are per pod, and Kubernetes balances connections rather than transactions,
  so one pod's pool can run dry while the other's is idle. A client queued
  longer than `query_wait_timeout` (30s) gets an error instead of waiting
  forever.
- **Packets.** `max_packet_size` is 64 MiB (one query or one result row), far
  below PgBouncer's 2 GiB default. kiln's own caps keep values well under it
  (32 MiB of extracted text, bounded request bodies), with one exception: the
  wiki's log artifact grows without bound, and a log row past 64 MiB would be
  refused. Bounding it is a follow-up.

### TLS

kiln's default `sslmode=require` encrypts the connection to the pooler but
does not verify its certificate. The chart's PgBouncer requires TLS from
clients (unless `config.database.sslmode` is `disable`), with a certificate
from `pooler.tls.secretName` — a cert-manager certificate, say — or, when that
is unset, a self-signed one the chart generates on first install and reuses on
upgrade (`helm template` without a cluster generates a new one each render, so
GitOps users should set `secretName`). Verifying it means `verify-ca` with
`database.sslrootcert` pointing at the CA, which the chart does not mount for
you yet. PgBouncer to Postgres uses `pooler.serverTlsSslmode` (`require`;
`verify-full` with `pooler.serverTLS.caSecretName`). The compose and CI
PgBouncer use `prefer` towards Postgres, because their Postgres has no TLS;
set `PGBOUNCER_SERVER_TLS_SSLMODE=require` when it is reached over a network.
Clients that have not finished logging in within 10 seconds are dropped
(`client_login_timeout`).

### Operating it

- A PgBouncer restart drops every client connection on that pod. kiln's pools
  reconnect, but a query in flight on a dropped connection fails, so expect a
  short burst of errors and reconnects. The chart's pooler container turns
  kubelet's SIGTERM into SIGINT, PgBouncer's safe shutdown (to PgBouncer
  itself, SIGTERM means "wait for every client to disconnect", which pooled
  kiln processes never do), after a 5s preStop sleep that lets the endpoint
  removal propagate. Running transactions finish first, within
  `pooler.terminationGracePeriodSeconds` (120s, which covers the sleep and the
  longest transaction). Compose stops it with SIGINT and a 120s
  `stop_grace_period`.
- A config change, or a change to a chart-owned password, rolls the
  PgBouncer pods (checksum annotations), since PgBouncer reads its config and
  userlist once at start. Rotating the password in an `existingSecret`, or
  renewing a `pooler.tls.secretName` certificate, needs
  `kubectl rollout restart deploy/<release>-pooler`.
- With a single pooler replica no PodDisruptionBudget is rendered: one that
  required the only pod would block every node drain.
- Follow-up: run the pooled pods as a non-owner database role, so the API and
  workers cannot alter the schema; today they use the migration role.

## Observability

Prometheus metrics are served on a separate port (`127.0.0.1:9090` by default; set `metrics_addr` to `:9090` so Prometheus can reach it from another pod or host, as the Helm chart does) by every
role: request traffic, queue depth, build outcomes, model spend, and whether
each scheduler duty is still succeeding. See [observability.md](observability.md) for the metric reference,
alert rules, and scraping queue depth to autoscale workers.

## Operating

**Upgrades.** Roll the migration Job, then the API, the workers and the
scheduler. Workers drain rather than abort, so a rolling update costs nothing
in flight; the scheduler holds nothing that needs draining.

**Rolling back a migration.** Every migration is additive, so the columns a
newer version added can stay; what stops the older binary is `schema_migrations`
holding a version it does not know, which it refuses at startup rather than run
against a schema it did not ship. Migration 021 (per-document ingest status)
is the current example: four nullable columns on `workspace_files`, no default,
no backfill. To roll back on one node: stop the new binary, run
`DELETE FROM schema_migrations WHERE version = 21`, start the old binary. On
Kubernetes, `helm rollback` runs no migrate hook (it is a pre-upgrade hook), so
delete the row before the old pods restart or they crash-loop on the schema
check. Nothing else is needed: the old code names its columns explicitly and
never reads the new ones. One thing to expect on rolling forward again: the old
binary stamps neither `content_changed_at` nor `staged_at`, so a document
replaced while it ran reads as "read" with its previous pages on the Ingest
page, and one it skipped reads as waiting, until the next ingest looks at the
bytes.

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
