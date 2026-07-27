# Roadmap

*The milestone plan `README.md` points at. Designed 2026-07-25 from a full-codebase audit;
update the status lines as milestones land.*

**Trajectory:** multi-tenant SaaS.
**Owner decisions:** all four priority areas (human loop, reading experience, automation, new
sources) are sequenced rather than narrowed; the embedded single-file UI evolves — no Next.js
rewrite until something on this list forces it.

**Guiding principles**

1. Activate dead-ready schema before adding tables. The database has described the destination
   since migration 001; most milestones are wiring, not invention.
2. Every security gate lands in the same milestone as the feature that makes it load-bearing,
   never after.
3. Deletion and destruction are always questions for a human, never side effects.

---

## M1 — Close the Human Loop, Fix the Reader ✅ shipped

*Shipped 2026-07-25 (PR #3). No new tables, no new infrastructure.*

- Agent-raised `ReviewFlag`s persist into `review_items`, deduplicated per open question
  (previously emitted on every run and silently discarded).
- A source that disappears from the map files a `kind='deletion'` review naming the pages at
  stake; the next build reads approved reviews back as its cascade authorization. `keep`
  authorizes nothing; dry runs file nothing.
- Write API v1 behind the pre-wired `write` scope: steering docs, page corrections
  (create/list/toggle), review list/resolve, backlinks. Guarded by RBAC (`org_members.role`;
  `viewer` is read-only, the legacy `member` default writes), a per-caller token bucket, and
  body caps (steering and corrections are prompt-injected verbatim — size is a cost boundary).
- Reader: pagination loop (fixed silent truncation at 500 pages and the false dead links it
  caused), backlinks panel from `page_links`, markdown tables/inline links/images/italics with
  scheme-checked URLs, `ts_headline` search snippets, review queue + steering + correction forms,
  mobile drawer nav, `If-None-Match` conditional requests, workspace persistence, sign-out.

## M2 — Server-Side Builds ✅ shipped

*Shipped 2026-07-26. Queue on the runs table, run-trigger API + dashboard,
DB-driven connectors with the LFI allowlist and sealed credentials, https
shallow clone, pyproject/Taskfile/Tiltfile parsers, and the upload-doc key
namespace (`doc:upload:<rel>`) that closed the path-collision caveat.*

*`kiln build` stops being CLI-only; the server can refresh its own wikis.*

1. **`kiln worker` + queue** — Postgres `FOR UPDATE SKIP LOCKED` over the existing `runs` table
   (add `status='queued'`, `claimed_by`, `claimed_at`). Partial unique index on
   `runs(workspace_id) WHERE status IN ('queued','running')`: one active run per workspace, and
   free webhook debouncing for M3. Refactor `runBuild` internals (`cmd/kiln/build.go`) into
   `internal/jobs` so CLI and worker share one entry point. An external queue is deliberately
   rejected: LLM-bound jobs at runs-per-hour throughput gain nothing from one and lose
   single-transaction claim+state.
2. **Run-trigger API + dashboard** — `POST/GET .../runs` (editor role, write scope, rate-limited —
   all inherited from M1). UI: runs list with status/cost, rebuild button. **Start reading
   `spend_ledger`** (written since day one, never read): rolling per-workspace budget windows
   enforced at enqueue. Write `run_items.est_cost_usd`; replace the hardcoded `$0.12/unit`
   estimate (`internal/jobs/pipeline.go`, `defaultEstimatePerUnit`) with a trailing average from
   `run_items` actuals.
3. **DB-driven connectors + remote clone + credentials** — activate the `connectors` table
   (admin-only CRUD), populate `sources.connector_id`. HTTPS shallow clone in
   `internal/connector/git/`. Activate `credentials` with the existing unused `internal/crypto`
   envelope encryption (write-only API; decryption only in the worker at sync time; first kind
   `git_pat`). Wire the `admin doctor` master-key check.
4. **Parser gaps** — pyproject.toml (detected but unparsed, `internal/mapper/repomap/scan.go`),
   minimal Taskfile/Tiltfile parsers, and the incremental-doc path-collision caveat flagged in
   `cmd/kiln/build.go` before automation makes it real.

**Security gates (strict order):** the **LFI allowlist of permitted roots lands before connectors
go DB-driven** — the code demands this itself (`internal/connector/git/git.go`,
`internal/connector/upload/upload.go`: connector `path` is a local-file-inclusion primitive the
moment configs stop being CLI-supplied). Default: deny local paths in server mode; CLI unchanged.
Remote-clone URL policy: https-only scheme allowlist, no redirects to local addresses, clone
depth/size caps, timeouts.

**Verification:** two workers + one queued run → exactly one claim; budget window exceeded →
enqueue refused with a review_item; remote clone of a public repo builds end-to-end; a
DB-configured `path` outside permitted roots is rejected.

## M3 — Multi-Tenant SaaS Shell ✅ shipped

*Shipped 2026-07-26. GitHub App sign-in with server-side sessions and CSRF,
webhook ingress with HMAC + cooldown, the worker's poll scheduler, org-owner
authority on admin surfaces with member management, and the auth-none
loopback refusal. The OAuth round trip is proven against a faked GitHub in
tests; a live browser round trip additionally needs a registered GitHub App
(client id/secret + private key in config).*

*Strangers can sign in, install the GitHub App, and their wikis stay fresh unattended.*

1. **GitHub App sign-in + sessions** — OAuth flow populates `users`
   (github_user_id/email/avatar); cookie-backed `sessions` (HttpOnly, Secure, SameSite=Lax) map
   into the same `auth.Identity` pipeline; bearer tokens remain for API/MCP clients. Installation
   flow activates `github_installations`/`user_installations`; short-lived installation tokens
   supersede PATs for GitHub clones.
2. **Webhook ingress** — `POST /hooks/github` outside `auth.Wrap`, behind HMAC verification,
   payload size cap, and its own rate limit: verify → map repo → enabled
   `trigger_mode='webhook'` connector → enqueue → 202. M2's unique index debounces push storms;
   add a completion cooldown. Poll scheduler in the worker for `trigger_mode='poll'`.
3. **Tenancy hardening** — enforce roles on admin surfaces; member management UI; **refuse to
   start `kiln serve` on a non-loopback address with `auth.mode=none`** (today that makes every
   caller an admin); **CSRF protection in the same PR that turns cookies on** (cookie auth makes
   the M1 write API CSRF-able for the first time).

**Depends on M2 (hard):** webhooks need the queue and remote clone to act on.

**Verification:** OAuth round trip in a browser; webhook with valid/invalid HMAC → 202/401; push
storm → one queued run; CSRF probe on a mutating route fails without the token.

## M4 — New Source Types & Compounding Polish ✅ shipped

*Shipped 2026-07-26. Web connector with the full SSRF posture
(resolve-then-connect pinning, https-only, size caps, no credential
forwarding), the graph view, budget warnings at 80% of the window, per-unit
cost attribution in the dashboard, and the EntryKey deletion.*

- **Web/URL connector** feeding the already-working `extract.FormatHTML`;
  `trigger_mode='poll'`. Ships with SSRF guardrails: deny private/link-local/metadata ranges with
  resolve-then-connect pinning, https-only, response size caps, no auth-header forwarding.
  Deliberately last — the largest new attack surface, benefiting most from M2/M3 hardening.
- **Graph view** in the UI from `page_links` (M1's backlinks proved the data; this is
  presentation, and the likely trigger for revisiting the single-file UI decision).
- Budget alerts as review_items when a workspace nears its spend window; per-page cost
  attribution in the dashboard.
- Cleanups: `diff.EntryKey` — use it or delete it.

**Verification:** web connector against a fixture site; SSRF probes (169.254.169.254, private
ranges, redirects to local) all rejected.

---

## Explicitly deferred

| Item | Why |
| --- | --- |
| `digests` table / notifications | Needs delivery infrastructure; little pull until M3 makes multi-user real. The natural first digest is a review-queue digest, post-M3. |
| Object storage (S3/MinIO), `blob_keys` / `DeleteBlobs` | Postgres comfortably holds page-sized text; config validation already exists, so later activation is cheap. |
| Next.js frontend | Owner decision: evolve `ui.html`. Every planned write flow is form+fetch shaped. Revisit when the graph view (M4) strains a single file. |
| External queue (Redis/NATS/SQS) | SKIP LOCKED covers the throughput profile by orders of magnitude; an external queue adds infra and dual-state failure modes for nothing. |
| Per-page live editing | Contradicts the architecture on purpose — pages are regenerated; `page_corrections` is the designed alternative. |
