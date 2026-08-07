# A local kiln on Docker Desktop's Kubernetes

One command brings up a persistent instance on the Kubernetes built into Docker
Desktop, with page generation running through the **Claude Code CLI** rather
than a direct Messages API call:

```bash
ANTHROPIC_API_KEY=sk-ant-... make k8s-up
```

It prints a URL and a bearer token. Paste the token when the UI asks.

This is the only entry point that exercises `agent.runner = "cli"` end to end.
`make dev` uses the fake runner and spends nothing; `docker compose up` and the
Helm chart both default to the API runner. Here the worker shells out to
`claude -p` against real sources, which is the path this stack exists to run.

**It spends real money.** The run budget defaults to $5.00 and the page cap to
10 per run. The per-call ceilings are raised above kiln's stock values, which
are sized for repository units: a document's extracted text rides in the prompt,
so one large PDF can exhaust the stock $0.40 analyze ceiling on its first call
and fail the unit *after* spending. Lower them all if you would rather be
interrupted than billed.

## What you need

* Docker Desktop with Kubernetes enabled (Settings → Kubernetes → Enable
  Kubernetes), and `kubectl` on `PATH` — Docker Desktop ships one.
* Model access for the CLI, one of:
  * `ANTHROPIC_API_KEY=sk-ant-...`, which kiln passes through to the
    subprocess as the only credential in its environment; or
  * `KILN_CLAUDE_CREDENTIALS_FILE=/path/to/.credentials.json`, a Claude Code
    session credential, for driving the CLI on a subscription. kiln passes
    `HOME` through to the subprocess precisely so this resolves.

    On macOS the CLI keeps this in the Keychain, not in a file, so there is
    nothing to point at until you export one:

    ```bash
    security find-generic-password -s "Claude Code-credentials" -w \
      > ~/.claude/.credentials.json
    chmod 600 ~/.claude/.credentials.json
    ```

    The credential is copied into the worker's `HOME` at startup rather than
    mounted, and that `HOME` is a volume — both deliberate. A secret mount is
    read-only, but an OAuth credential is not read-only data: the access token
    is short lived and the CLI is expected to spend the refresh token and write
    a new pair back. Mounted read-only it cannot, and the first expiry is fatal;
    on an ephemeral `HOME` the refresh is lost on restart and the *second*
    deploy fails, because refresh tokens rotate and the exported one has already
    been spent.

    Because `HOME` persists and is never overwritten, a re-export needs a nudge:

    ```bash
    KILN_CLAUDE_CREDENTIALS_FILE=~/.claude/.credentials.json make k8s-up
    make k8s-reauth      # discard the stored credential, re-seed from the secret
    ```

    It also copies a live credential onto disk and then into a Kubernetes
    Secret, so it exists in two more places than it did. **The API key is the
    path that does not decay, and the one this stack is tested against.**

Without either, everything comes up and builds fail at the first page with
*"Not logged in"*. `make k8s-up` warns about this before it starts.

## Commands

| Command | Does |
| --- | --- |
| `make k8s-up` | Build, load, apply, wait. Idempotent — this is also the upgrade command. |
| `make k8s-down` | Stop the workloads. **Keeps the data.** |
| `make k8s-purge` | Delete the namespace and every volume in it. |
| `make k8s-status` | Pods, services, volumes. |
| `make k8s-logs` | Follow the worker. `make k8s-logs C=api` for the API. |
| `make k8s-token` | Mint another bearer token. |
| `make k8s-sync SRC=path` | Copy a local directory in, to ingest as `/sources/<name>`. |
| `make k8s-reauth` | Replace the worker's stored Claude Code credential with the secret's. |
| `make k8s-shell` | A shell in the worker pod. |

Everything is `scripts/k8s-local.sh` underneath, which takes the same verbs.

## What it creates

In namespace `kiln-local`:

| Object | Purpose |
| --- | --- |
| `StatefulSet/postgres` | Postgres 16 on an 8Gi claim. The Helm chart assumes a managed database; a laptop has none. |
| `PVC/kiln-blobs` | 10Gi for uploads and run artifacts, `storage.backend = "fs"`. |
| `PVC/kiln-sources` | 5Gi staging area for local repositories and documents. |
| `PVC/kiln-home` | 1Gi for the worker's `HOME`, so a refreshed Claude Code session credential survives a restart. |
| `Job/kiln-migrate` | `kiln admin migrate`. Advisory-locked, so re-running is safe. |
| `Deployment/kiln-api` | API and reading UI. |
| `Deployment/kiln-worker` | The build worker, with the Claude Code CLI on its `PATH`. |
| `Service/kiln` | `LoadBalancer`, published on `localhost:8080`. |
| `Secret/kiln-secrets` | Master key, Postgres password, database URL, API key. |
| `ConfigMap/kiln-local-state` | Which node the volumes live on, and which image it holds. |

The image is built here, not pulled: `deploy/local-k8s/Dockerfile` layers Node
and `@anthropic-ai/claude-code` onto the repository's runtime image. The
production image deliberately does not carry them — a deployment on the API
runner should not ship a Node runtime it never executes.

## Persistence

`make k8s-down` deletes Deployments, the StatefulSet and the Job, and nothing
else. Claims, Secrets and ConfigMaps stay, so `make k8s-up` comes back to the
same wiki, the same master key, and the same tokens. Only `make k8s-purge`
destroys data.

Two caveats worth knowing before you rely on it:

* Resetting Kubernetes in Docker Desktop deletes the volumes with it. The
  stack notices — it refuses to start on a node its data no longer lives on —
  but it cannot bring the data back.
* The master key is generated once and kept in `kiln-secrets`. It seals
  connector credentials at rest, so purging the namespace makes any stored
  credential unrecoverable, exactly as losing it would in production.

## Ingesting a local repository

Docker Desktop's Kubernetes is a multi-node cluster whose nodes are containers
with no view of the host filesystem, so a `hostPath` mount cannot reach a
repository on your Mac. Copy it in instead:

```bash
make k8s-sync SRC=~/code/my-project     # lands at /sources/my-project
```

Then add it as a source in the **Sources** view with path
`/sources/my-project`, or over the API:

```bash
TOKEN=$(cat .dev/k8s/token)
curl -X POST localhost:8080/api/v1/workspaces/my-project/connectors \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"git","name":"local","config":{"path":"/sources/my-project"}}'
```

`/sources` is the worker's entire `worker.permitted_source_roots` allowlist, so
a connector config — which is API-writable data — cannot point it anywhere else
in the container.

The copy is a snapshot, not a mount. Re-run `k8s-sync` after the source changes,
then build again. Remote sources have no such problem: an https git remote, a
document upload, or a web page needs none of this.

## Configuration

Read from the environment by `make k8s-up`:

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` | — | Model access. Also read from `KILN_ANTHROPIC_API_KEY`. |
| `KILN_CLAUDE_CREDENTIALS_FILE` | — | A Claude Code session credential, instead of a key. |
| `KILN_K8S_PORT` | `8080` | Host port for the UI. |
| `KILN_K8S_NAMESPACE` | `kiln-local` | Namespace. |
| `KILN_K8S_CONTEXT` | `docker-desktop` | kubectl context. |
| `KILN_AGENT_MODEL` | CLI default | Model override. |
| `KILN_AGENT_BASE_URL` | `$ANTHROPIC_BASE_URL` | Gateway or proxy endpoint, reaching the CLI as `ANTHROPIC_BASE_URL`. Inherited from your shell, since a key scoped to a gateway is usually already configured that way. |
| `KILN_AGENT_RUN_BUDGET_USD` | `5.00` | Per-run spend ceiling. |
| `KILN_AGENT_MAX_PAGES_PER_RUN` | `10` | Pages per run. |
| `KILN_AGENT_ANALYZE_BUDGET_USD` | `1.00` | Per-analyze-call ceiling. Above kiln's stock `0.40`, because a document's extracted text rides in the prompt and one large PDF can exceed it on the first call. |
| `KILN_AGENT_PAGE_BUDGET_USD` | `2.00` | Per-page-call ceiling. |
| `KILN_CLAUDE_CODE_VERSION` | `latest` | Pin the CLI version in the image. |
| `KILN_LOG_LEVEL` | `info` | |

An unset `ANTHROPIC_API_KEY` keeps whatever is already in the cluster rather
than blanking it, so rotating a key is just
`ANTHROPIC_API_KEY=... make k8s-up`.

## Why it is shaped this way

Three properties of Docker Desktop's cluster drive most of the design, and each
is worth knowing before changing anything here.

**Its nodes do not share the host's image store.** They run their own
containerd, so a locally built image is invisible until it is exported and
imported. `scripts/k8s-local.sh` does that with `docker save` piped into
`ctr -n k8s.io images import`, and skips it when the node already holds the
image — the export is most of a gigabyte.

**Its default StorageClass binds a volume to one node.** `local-path` with
`WaitForFirstConsumer`, and the claims here are `ReadWriteOnce`. Combined with
the point above, every kiln workload is pinned to a single node, recorded in
`kiln-local-state` so restarts land where the data already is. That is also
what makes one `ReadWriteOnce` blob claim shared by the API and the worker
legal: the restriction is per-node, not per-pod.

**A `NodePort` is not reachable from the host, but a `LoadBalancer` is.** Docker
Desktop publishes a LoadBalancer's port on `localhost`; on its multi-node
cluster a NodePort answers only from inside the cluster network. Hence
`Service/kiln` is a LoadBalancer even though nothing here is balancing
anything.

Two smaller ones:

* The manifest is applied in two passes, split on a `# @stage workloads`
  comment. Both roles check the schema version on boot and exit if it is not
  the one they were built against, so applying everything at once works only by
  way of a CrashLoopBackOff that resolves a minute later.
* Auth is on. Inside a pod the server must bind every interface to be
  reachable at all, and kiln refuses to serve a wide-open bind with auth
  disabled. `auth.mode = "none"` stays a localhost convenience for `make dev`.

## This is not the production path

For a real deployment use the Helm chart at [`../helm/kiln`](../helm/kiln),
which assumes a managed Postgres and S3-compatible object storage, runs the API
and workers as independently scalable Deployments, and does not pin anything to
a node. See [`docs/deployment.md`](../../docs/deployment.md).
