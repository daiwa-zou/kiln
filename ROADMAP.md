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

Still open: activate `digests` for review-queue notifications, and revisit
the permissive `member`-writes default (members write content; owners shape
connectors, credentials, and membership -- the split holds, but the default
role granted on sign-in deserves a second look).

## Deferred decisions

| Item | Trigger to revisit |
| --- | --- |
| Framework frontend | SaaS onboarding flows (M8) making server-driven UI painful |
| `workspaces.model` / per-bench model override | First user who needs Opus on one bench and Sonnet on the rest |
| Incremental doc/web change detection | A docs source large enough that full re-extraction is the slow step |
