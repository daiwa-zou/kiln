# Roadmap

*Second roadmap, designed 2026-07-27 from a full-codebase audit after the
first plan (M1–M4, designed 2026-07-25) shipped in full. Update the status
lines as milestones land.*

**Trajectory:** better wikis first — output quality and freshness lead; SaaS
completion follows once the content engine deserves the audience.

**Guiding principles**

1. Activate dead-ready code before writing new. The audit found a complete,
   tested incremental-routing engine that production never feeds, retention
   knobs with no sweeper, and validated storage config with no client.
2. Every security gate lands in the same milestone as the feature that makes
   it load-bearing, never after.
3. Deletion and destruction are always questions for a human, never side
   effects.
4. The hash gate is the correctness guarantee; change routing is an
   optimization layered on top. Anything that loses a commit range must
   degrade to a wider rebuild, never to a stale page.

---

## Shipped (first roadmap, 2026-07-25 → 2026-07-26)

- **M1 — Human loop + reader** (PR #3): review queue, steering, corrections,
  write API v1, reader overhaul.
- **M2 — Server-side builds** (PR #8): Postgres run queue (`FOR UPDATE SKIP
  LOCKED`), `kiln worker`, run API + dashboard, budget windows, DB-driven
  connectors behind the LFI allowlist, sealed credentials, https shallow
  clone, parser gaps.
- **Fake runner + dev env** (PR #9): `agent.runner = "fake"` for zero-cost
  end-to-end runs; `kiln_dev` isolation from the test database.
- **M3 — Multi-tenant SaaS shell** (PR #10): GitHub App sign-in, revocable
  cookie sessions + CSRF, webhook ingress with HMAC + storm debounce +
  cooldown, poll scheduler, org-owner authority, member management,
  auth-none loopback refusal.
- **M4 — Sources & polish** (PR #11): web connector with the full SSRF
  posture (resolve-then-connect pinning), graph view, budget warnings at
  80%, per-unit cost attribution.

## M5 — Make Incremental Real ✅ shipped (2026-07-27, PR #12)

*The `internal/diff` router is complete, tested, and never constructed with
anything but `FullRebuild: true`. A one-file push still maps and hashes every
unit; webhooks discard GitHub's file lists; `--depth 1` clones cannot diff.*

1. **`diff` detection** — a git connector function derives `[]diff.Change`
   from `git diff --name-status <from>..<to>`; `detect.go` finally earns the
   derivation its doc comment promises. Fallback is always `FullRebuild`
   (first build, force push, unknown refs, non-git sources).
2. **Ranges through the queue** — migration 004 adds `runs.ref_to` and
   `runs.not_before`. The webhook records the pushed head; **`ref_from` is
   deliberately not stored** — the worker derives it from the workspace's
   last successful build ref at claim time, so debounced or dropped pushes
   can never leave a hole in the range. The completion cooldown becomes
   `not_before` scheduling: pushes always enqueue (the active-run index still
   debounces), a debounced push updates the queued run's `ref_to`, and the
   claim query simply skips runs whose time has not come.
3. **Fetchable clones** — deepen the shallow clone just enough to diff the
   last-built ref (bounded `--deepen`, then fetch-by-sha where the host
   allows it); every existing clone guardrail stays. Missing history →
   FullRebuild, never an error.
4. **Continuation runs** — units deferred by `agent.max_pages_per_run`
   currently stay stale until someone notices; the worker now enqueues a
   follow-up run (respecting `not_before`) until the plan drains.
5. **Honesty fixes** — the stale `cmd/kiln/main.go` worker comment, the
   `admin doctor` storage overclaim, and the README's incremental wording.

**Verification:** a push touching one module regenerates exactly that module
(plus arch when manifests changed), proven by run_items; a push storm during
cooldown yields one queued run whose `ref_to` is the newest head; force-push
falls back to FullRebuild cleanly; upload/web document keys are untouched by
git ranges (the `UploadOrigin`/`WebOrigin` namespace invariant holds).

## M6 — Content Quality & Correctness ✅ shipped (2026-07-27, PR #13)

1. **Deletion blind spot** — a disappeared source raises its review even when
   no same-prefix units survive (today the last doc of a kind vanishes
   silently; `internal/jobs/collect.go`).
2. **Per-source connector attribution** — replace the run-wide
   `ImportRequest.ConnectorID` simplification; make `sources.connector_id`
   clearable so connector swaps cannot leave stale attribution.
3. **Batch path-collision validation** — two units in one run must not claim
   the same page path; enforced in `wiki.ValidateBatch`, not documented in a
   runner comment.
4. **Graph at scale** — LIMIT + top-N-by-links server-side (the one
   unpaginated read), node cap + async layout client-side.
5. **UI structure decision** — the single-file revisit trigger has fired
   (2,057 lines, graph shipped). Split `ui.html` into a multi-file
   `embed.FS` with the same CSP hashing; a framework frontend stays deferred
   until SaaS onboarding demands it.

## M7 — Lifecycle Hygiene ✅ shipped

*Shipped 2026-07-27. Drain-with-grace requeues interrupted builds, the GC
sweeper enforces the retention knobs that waited since M0, admin surfaces
require the new admin scope, and `kiln admin rotate-key` re-seals
credentials atomically.*

1. **Worker drain** — SIGTERM mid-build requeues the in-flight run after a
   grace window instead of failing it (today: wasted spend, stale bench);
   `serve --with-worker` waits for the worker before exiting.
2. **GC sweeper** — enforce `soft_delete_retention` (the purge index exists
   with no purger), sweep expired sessions and revoked/expired tokens, and
   prune old runs/run_items; `spend_ledger` keeps a longer horizon because
   budget windows read it.
3. **`admin` scope** — split from `write` so a steering-editing token cannot
   seal credentials or reshape connectors; `tokens.scopes` is already an
   array, so this is code, not migration.
4. **Master-key rotation** — `kiln admin rotate-key` re-seals every
   credential under a new key in one transaction; the worker's error text
   already asks the question the command answers.

## Sources self-serve ✅ shipped (2026-07-27)

*Users and admins set sources from the UI, and documents upload from the
browser. Pulled forward from M8 because the connector CRUD API had shipped
in M2 with no UI over it — sources were configured with `curl`.*

1. **Object storage client** (`internal/blob`) — the parked M8 item: an fs
   driver (os.Root-jailed, atomic writes) and an S3/MinIO driver behind the
   long-validated `storage.*` config; blob GC consumes the cascade's
   `DeleteBlobs` after import.
2. **Browser upload** — `workspace_files` (migration 005) + a streaming
   multipart `POST …/files` (32 MiB cap, extract-format allowlist, blob keys
   server-constructed per workspace). Upload/delete are member-level writes;
   page deletion still routes through the review queue.
3. **Files-mode upload connector** — an upload connector with no `path`
   consumes the workspace's uploaded files; the worker materializes blobs to
   a staging dir, so the connector and the LFI allowlist are untouched.
4. **Git-optional builds** — a bench fed only by documents or web pages
   builds; synced-namespace bookkeeping keeps un-scanned repo sources from
   reading as deletions, and no phantom `arch:overview` is planned.
5. **Sources view** — connector CRUD, credential sealing, drag-drop
   document upload, and Build-now in the reader UI (`ui/sources.js`).

## M8 — SaaS Completion ✅ mostly shipped (2026-07-30)

1. **Deployable artifact** — a non-root multi-arch image on GHCR (signed,
   with SBOM and provenance), a compose stack that runs the real topology
   (API and a scalable worker pool against Postgres and S3), and a Helm
   chart with separate API/worker Deployments, a migration hook, HPAs, and a
   restricted pod security context. `docs/deployment.md`.
2. **`/metrics`** — Prometheus on its own listener for both roles: request
   traffic, build outcomes, queue depth, and model spend, with bounded label
   cardinality. Queue depth is the worker autoscaling signal.
   `docs/observability.md`.
3. **Self-serve bench creation** — `POST /api/v1/workspaces` makes the
   creator an owner of the org, closing the "user #2 signs into an empty
   list with no path forward" gap. Creating inside an existing org requires
   owning it, and answers 404 either way so orgs cannot be enumerated.
4. **WCAG 2.2 AA** — audited across every view in both themes; contrast,
   target sizes, and accessible names fixed. `docs/accessibility.md`.

Still open, and now planned as M11 items 1 and 2: activate `digests` for
review-queue notifications, and revisit the permissive `member`-writes
default (members write content; owners shape connectors, credentials, and
membership -- the split holds, but the default role granted on sign-in
deserves a second look).

## Target deployment architecture

*Recorded 2026-10-10. The topology M9 and M10 build toward, so each item
below has a picture to land in. Same image, same roles, same schema
throughout — scaling never means a new service. How to run each component
today: [docs/deployment.md](docs/deployment.md).*

**Small team or single tenant: one VM.** Docker Compose with `api`, one or
two `worker` replicas, and Postgres 16. Blobs go to real S3 or R2 rather
than the bundled MinIO, so documents outlive the VM. TLS terminates at
Caddy or nginx with `client_max_body_size 34m`. Backups are a nightly
`pg_dump` plus bucket versioning, and `KILN_MASTER_KEY` is stored off the
VM — losing it loses every sealed credential. Builds are model-bound, not
CPU-bound, so this tier carries more than it looks; leave it for high
availability or more than a few workers.

**Cloud: managed Kubernetes plus managed services.** The Helm chart as it
ships, with these choices around it (AWS names; GCP equivalents in
brackets):

```
users, agents ──► CDN ──► load balancer / ingress ──► kiln serve (2–N, HPA, PDB)
                  caches /ui/*                              │
GitHub webhooks ───────────────────────────────────────────►┤
                                                            ▼
                                  pooler (PgBouncer / RDS Proxy) ──► Postgres 16
                                                            ▲        (Multi-AZ, PITR)
                    kiln worker (1–N) ──────────────────────┘
                      │ egress only: Anthropic API, git hosts, web sources
                      └──► S3 bucket (workload identity, versioned)
Prometheus ──► metrics listener (cluster-internal)
```

- **Database** — RDS [Cloud SQL] Postgres 16, Multi-AZ, point-in-time
  recovery, behind a transaction-mode pooler. The one component that must
  be highly available; everything else is stateless.
- **Object storage** — S3 [GCS interop] with versioning; storage keys left
  unset so pods use IRSA [workload identity].
- **API** — `kiln serve`, at least two replicas with the chart's
  PodDisruptionBudget, autoscaled on CPU. A CDN in front caches `/ui/*`
  today and page reads once M10 item 2 sends `Cache-Control`.
- **Workers** — `kiln worker`, outbound-only. Egress is limited to the
  model API, git hosts, and web sources; the SSRF guard still applies
  inside that. A fixed count until queue-depth autoscaling (M10 item 4).
- **Secrets** — a secret manager mounted as files and read through the
  `_FILE` variables, never inline environment values.
- **Metrics** — the separate listener stays cluster-internal.

**Interim settings while M9 is open.** Each open item has a setting that
makes the cloud tier safe today, and is retired by the item:

| Open item | Until it lands |
| --- | --- |
| M9·1 Connection pooler | The chart's ceilings (10 API × 20 conns, 20 workers × 6) reach 320 connections. Put RDS Proxy or PgBouncer in front, or keep `maxReplicas` low enough to fit `max_connections` |
| M9·2 Real client addresses | Every anonymous caller behind the load balancer, GitHub included, shares one rate-limit bucket. Raise the API's limits and rate-limit at the load balancer or WAF |
| M9·5 Scheduler role | Polling, sweeps, and requeues run in every worker. Harmless at a handful of workers; hold the worker count there |

**After M10.** The API runs as three profiles of one image (reader,
control, ingress — uploads possibly a fourth), each with its own
Deployment and HPA; the CDN absorbs most reads; workers scale on queue
depth, down to zero.

## M9 — Make Builds Parallel — items 3–4 shipped (2026-07-30, PR #27); item 2's webhook half shipped (PR #87); 1, 2's `RealIP` half, and 5 open

*Items 3 and 4 shipped together: `agent.unit_concurrency` drives the bounded
pool in `internal/jobs/fanout.go`, and the run budget is a reservation taken
before each call. PR #87 keyed verified webhook deliveries on their
repository or installation (`hookSubject` in `internal/api/webhook.go`), so
the cross-tenant 429 is gone. Items 1 and 5, and item 2's `RealIP` half,
have no code yet, and they are now the urgent ones — raising concurrency invites adding worker pods, which is
exactly what the pooler gates and what multiplies the per-worker timers.*

*Designed 2026-07-30 from a scaling review. The roles are already split and
already scale independently; what does not scale is one bench. A run walks
its units in a plain loop, the run is capped at twelve, and the partial
unique index allows one active run per workspace — so a 120-unit first
build is 240 sequential model calls pinned to a single worker, and adding
workers does nothing for it. Nothing in this milestone changes the
topology; every item pays off at the shipped two-role deployment.*

1. **Connection pooler first** — the API pool is 20 per replica against an
   untuned Postgres default of 100, and the configured autoscaling ceilings
   (10 API, 20 worker) demand 320. PgBouncer in transaction mode, plus
   single-digit per-replica pools. This gates everything below it: every
   later item adds pods, and adding pods today buys `FATAL: sorry, too many
   clients already`.
2. **Real client addresses** — `limiterKey` falls back to `r.RemoteAddr`
   with no `RealIP` middleware anywhere, so behind an ingress every
   unauthenticated caller shares one bucket. Add trusted-proxy `RealIP`
   for that path. The webhook half shipped in PR #87: a delivery whose
   HMAC verifies is limited on its repository (else its installation), the
   correct tenant boundary regardless of replica count, and only
   unverified deliveries still share the peer-address bucket.
3. **Intra-run unit fan-out** — the milestone's reason to exist. Generate
   units concurrently against a snapshot with a bounded per-workspace pool,
   then run collision detection, link validation, and `Import` serially.
   Validating wikilinks after every unit lands is not just a concession to
   concurrency: today unit 3 cannot link to a page unit 7 is about to
   write, and resolving against the full run fixes that.
4. **The budget gate survives concurrency** — landing with item 3, not
   after it. The run budget is enforced by a sequential accumulator: each
   unit is handed `RunUSD - spent` and stops itself there. With C units in
   flight against one snapshot of `spent`, worst-case overshoot is C times
   the per-unit budget. A shared atomic reservation taken before each call,
   not after it, keeps the ceiling a ceiling.
5. **Scheduler as a singleton role** — `requeueStale`, `pollSources`,
   `sweep`, and `sampleQueueDepth` run inside every worker's claim loop on
   in-process timers. At thirty workers that is thirty concurrent hourly GC
   sweeps racing the same rows and a requeue query every five seconds per
   pod. Extend the existing `config.Role` enum with a scheduler role at
   `replicas=1`; workers become claim-and-build only.

**Verification:** a 60-unit run at concurrency 8 finishes in roughly an
eighth of the wall clock of the same run at concurrency 1, proven by
`run_items` timestamps, with byte-identical pages; two units claiming one
page path still fails the later unit and leaves the earlier page intact; a
wikilink to a page written by another unit of the same run resolves rather
than validating as dead; a run canceled mid-flight still imports completed
units in one transaction and is still ledgered; a run with `run_budget_usd`
set never exceeds it at any concurrency, checked against the ledger rather
than the summary; with the scheduler running and workers at zero, the
queue-depth gauge is still published; with N workers, exactly one GC sweep
occurs per hour.

## M10 — Split the Serving Tier

*Only worth starting once M9 lands — the pooler is the prerequisite, and
the fan-out is the larger win. `serve` currently answers page reads, 32 MiB
uploads, webhook bursts, OAuth, and the embedded UI from one process and
one pool, so a slow upload and a page view contend for the same resources.
The split is deployment profiles of one binary, not new services: one
schema, one atomic `Import`, one queue.*

1. **Identity cache** — `auth.Wrap` resolves an identity from Postgres on
   every request, deliberately, so revocation is immediate. A 5–10s cache
   with an explicit revocation bump keeps that property and removes most of
   the read tier's load.
2. **Content caching** — pages change only when a build imports them.
   `Cache-Control` keyed on the workspace's last run id lets a CDN or
   reverse proxy absorb the reading traffic; UI assets are already
   content-hashed with ETags computed at init, so they need only be fronted.
3. **Route-subtree roles** — `serve --role=reader|control|ingress`
   selecting mount subtrees, then three Deployments with their own limits
   and HPA policies. Uploads likely deserve a fourth profile: they hold a
   connection for 32 MiB at a time.
4. **Queue-depth autoscaling** — the signal `values.yaml` already
   recommends and the HPA does not yet use. Safe only once the scheduler
   (M9) publishes the gauge, since today it comes from the very pods being
   scaled and vanishes at low replica counts.
5. **Workload classes** — a `runs.class` column and a claim-query filter,
   giving small and large worker pools so one large bench cannot monopolize
   the fleet. Deferred within the milestone until it is a real complaint.
6. **Network policy in the chart** — the target architecture's worker
   egress limit and private metrics listener exist only as advice; the
   chart ships no `NetworkPolicy`. Add one per role, off by default: the
   API accepts ingress on its HTTP port only, metrics accept only the
   configured scraper namespace, and workers get an egress allowlist
   (DNS, Postgres, the pooler, object storage, and configurable
   destinations for the model API and sources).

**Verification:** a sustained read load against the reader profile shows
auth queries falling by roughly the cache hit rate with no change in
revocation latency past the TTL; a 32 MiB upload in flight does not raise
page-read latency on the reader pods; a webhook storm against one
installation does not 429 another's; queue depth drives worker replicas up
from zero and back down without evicting an in-flight build; with the
network policies enabled, a worker cannot open a connection to a
destination outside its allowlist, and a pod outside the scraper namespace
cannot read `/metrics`.

## M11 — The Edges Catch Up — items 3 and 9 shipped (2026-10-10)

*Designed 2026-10-10 from a codebase review. The build engine is the
strongest part of kiln; what lags is around it — nobody is told when the
wiki needs them, agents can read but not report, a regenerated page
erases its own past, and the reader still shows its plumbing (slugs,
citation chips, a status that contradicts the pages). Starts once M9's
remaining items land (item 5 hosts item 2 below); it comes ahead of M10,
which waits for read traffic to ask for it. Item 9 depended on nothing and
landed first.*

1. **Default sign-in role is configurable** — `auth/pg.go` inserts the
   sign-in membership with the column default, `member`, which writes
   content and steering. Add `auth.default_role` (`viewer` | `member`),
   defaulting to `member` so upgrades change nothing; hosted deployments
   set `viewer`. Closes the M8 open question without deciding it for
   everyone.
2. **Digests** — the `digests` table (`001_initial.sql`: channel, target,
   cadence, enabled, last_sent_at) has had no reader or writer since M0.
   The scheduler role sends, per enabled row, what changed since
   `last_sent_at`: new review items, failed or partial runs, budget
   warnings. Channels: an SMTP sender and a Slack-compatible incoming
   webhook. Delivery targets are owner-set and pass `netguard` like any
   other outbound URL. An outbound `run.finished` webhook rides the same
   plumbing.
3. **MCP write tools** — all seven tools read. Add, behind the `write`
   scope: `request_gap` (the existing `POST …/gaps/{slug}/request`),
   `file_question` (a review item of kind `uncertain` or `contradiction`,
   naming a page), and `propose_correction` (files a review item carrying
   the proposed text; a human promotes it to a pinned correction).
   Nothing an agent sends changes a page directly — deletion and
   correction stay questions for a human (principle 3).
   *Shipped, corrected as built:* the tools sit behind a new
   `propose:<bench>` scope, not `write`, because a write token can already
   pin corrections, resolve deletion reviews and delete pages over REST.
   Agent keys get `read` + `propose:<bench>` ("Can file questions and
   corrections"), pinned to one bench and admitted on exactly `POST
   …/reviews` and `POST …/gaps/{slug}/request`. `review_items` gains
   `proposed` (the correction text), `origin` (the filer's name, display
   only), `origin_kind` (key, session or local) and `origin_token` (a plain
   id, so a swept key's items stay capped); `page_corrections` gains
   `proposed_by`, and prompts label a promoted correction as agent-proposed
   and member-confirmed. Promotion is a separate human route, `POST
   …/reviews/{id}/promote`, which needs `write` and pins the text the person
   confirmed. Caps, gap requests included: 20 open items per key, 200
   key-filed per bench, a per-key filing bucket on top of the user's.
   *Follow-up:* write tokens are user-wide, so a write token handed to an
   agent is the cross-bench confused-deputy surface that propose keys avoid —
   pinning write tokens to benches is its own decision.
4. **Page revisions** — `WikiStore.Import` overwrites a page in place, so
   history exists only where a publish target mirrors it to git. Write a
   `page_revisions` row inside the same `Import` transaction (previous
   body, run id, the run's `ref_to`), show a diff in the reader, and let
   the GC sweep enforce a retention window. The hash gate means unchanged
   pages write no revision.
5. **Search, step two** — the `pg_trgm` fallback from
   [storage-and-retrieval.md](docs/storage-and-retrieval.md) (option B) for
   queries the OR+rank pass leaves empty, and an alert on the
   `kiln_searches_total{outcome="empty"}` rate whose threshold is written
   down as the pgvector trigger.
6. **Dry run over the API** — the pipeline already plans and estimates
   without spending (`kiln build --dry-run`). Expose it as
   `POST …/runs?dry_run=1` and show the plan and estimate behind Build
   now, so a large first build is never a surprise.
7. **Web change detection, the cheap half** — the web connector fetches
   exactly the URLs it is given. Send `If-None-Match` / `If-Modified-Since`
   from the last fetch, and discover URLs from a `sitemap.xml` under an
   explicit page cap, both inside the existing SSRF posture.
8. **A typed, component UI that still ships in the binary** — the reading
   UI is ~6,400 lines of hand-written JavaScript, 4,900 of them in
   `ui/app.js`, and items 2, 4, and 6 each add a view (digest settings, a
   page diff, a plan preview). Move it to TypeScript and a small component
   library (Preact or Svelte), bundled by Vite into content-hashed static
   files that `embed.FS` serves exactly as today — same ETags, same
   `script-src 'self'` CSP, no inline scripts. The built output is
   committed, so `go build` and every Go-only CI job still need no Node;
   a CI job rebuilds it and fails on any diff, the way `go mod tidy` is
   checked. API response types are generated from the Go handlers' structs,
   so contract drift fails the build rather than a page. Port view by view
   behind the same routes, re-running the WCAG 2.2 AA checks from
   [accessibility.md](docs/accessibility.md) on each. Lands before the
   views it exists for; a Next.js server is ruled out (see Deferred
   decisions). The graph is the view to port with the most to gain: its
   first layout fills about a third of the canvas and its labels overlap.
   Fit the layout to the viewport on load, label nodes by title, and hide
   colliding labels until zoomed in.
9. **The reader reads like prose** ✅ shipped (2026-10-10) — fixes from a
   UI review on 2026-10-10, small enough to land in today's `app.js` ahead
   of item 8. The port carries them over unchanged.
   - *Wikilinks show titles.* An unaliased `[[slug]]` renders its target's
     title from `state.pages`; a link to a page that does not exist keeps
     its slug and its dashed underline.
   - *Citations stop interrupting.* Every `(file.md:1–8)` rendered inline
     as a code chip, several per paragraph. Each — and each code span that
     is nothing but a citation — is now a numbered
     superscript marker — a 24px button — that opens a card naming the
     source's path and lines on hover, keyboard focus or tap (there is no
     route that shows a source file itself), and the page's Provenance
     block lists every cited file with its lines, numbered to match.
     Groups that mix a citation with prose, and citations inside link text
     or alt text, stay as written. So does a citation hard-wrapped onto a
     list item's continuation line, which the renderer sets as a block of
     its own.
   - *The deck is not the first sentence twice.* The opening sentence that
     becomes the deck is dropped from the body, and its citations are
     stripped from the deck rather than mangled (`md:1 8`).
   - *The run pill tells the truth.* A bench with pages and no runs says
     when its pages were last written and that they came from outside the
     queue, in the pill, its popover and Ingest — never "No builds yet" or
     "No runs yet".
   - *Phones keep status and the account.* Below the mobile breakpoint the
     run pill and the account button stay visible at 24px or wider (the
     search field becomes an icon button to make room); on desktop the
     account button shows initials or a person glyph and a word.
   - *Hover is never the only way in.* The Index writes each page's
     freshness beside its title, and rail links carry a `title` with the
     full title and freshness.
   - *A missing page offers a way on.* A dead `#/page/…` hash offers a
     search for the slug, the nearest titles and the Index; a failed fetch
     still shows the error with Reload.

**Verification:** with `auth.default_role = "viewer"`, a fresh sign-in can
read but every write answers 403; a review item filed after a digest's
`last_sent_at` appears in exactly one digest and a disabled row sends
nothing; a digest target resolving to a private address is refused; an MCP
`propose_correction` under a read-scoped token is refused, and under a
write-scoped token creates a review item while the page body is unchanged;
a run that regenerates one page writes exactly one revision and a no-change
run writes none; a misspelled keyword (`kubernets`) finds its page; a dry
run over the API creates no `runs` row and no ledger entry; a web source
answering `304` makes no extraction and plans no unit; `go build` on a
machine without Node produces a binary whose UI works, the UI is served
under the unchanged CSP with no violations in the console, a changed API
field that the UI reads fails type-checking, and every ported view passes
the same accessibility audit in both themes; a page's unaliased wikilinks
show their targets' titles, its body holds no inline citation chips, and its
deck sentence appears once; a bench built only from the CLI never shows "No
builds yet"; at 375px the run pill and account button are visible and at
least 24px wide; the graph's first frame shows every node with no
overlapping labels.

## Deferred decisions

| Item | Trigger to revisit |
| --- | --- |
| Server-rendered frontend (Next.js or similar) | Wikis meant to be public and found by search engines. Today every bench sits behind a token, so server rendering buys nothing, and a Node server breaks "one binary plus Postgres". If it comes, `next build` with `output: 'export'` keeps the embed; a Node runtime needs its own case. The framework question itself is settled by M11 item 8 |
| `workspaces.model` / per-bench model override | First user who needs Opus on one bench and Sonnet on the rest |
| Incremental doc/web change detection | A docs source large enough that full re-extraction is the slow step (the web half's cheap part is M11 item 7) |
| pgvector hybrid search | The empty-search rate crossing the threshold M11 item 5 writes down. Plan in [storage-and-retrieval.md](docs/storage-and-retrieval.md), step 3 |
| OIDC sign-in beside GitHub | A deployment whose members are not on GitHub — likely a documents-only team. Reuses the session and CSRF machinery |
| SaaS document connectors (Confluence, Notion, Google Drive) | A named user asking for one; build that one first |
| GitLab / Bitbucket webhooks | A git source hosted there that polling makes too stale |
| Agent-filed `query` pages | MCP write tools (M11 item 3) in use, and a way to tell a good answer from a plausible one |
| External broker for the run queue | Postgres queue contention that `SKIP LOCKED` and workload classes cannot absorb. Costs the transactional debounce and exactly-once claim, so the contention has to be measured first |
| Per-domain services with their own datastores | A domain whose write volume genuinely cannot share Postgres. Costs the atomic `Import` — pages, sources, index, overview, and log in one transaction — which is what makes navigation unable to drift |
