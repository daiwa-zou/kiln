# kiln — Code Review

*Reviewed 2026-07-25, at commit `17a06d2`. ~16.5K lines of Go. `make test` and `go vet ./...` pass clean at this baseline.*

> **Status: all findings implemented, 2026-07-25.** Every item below has been addressed in the working tree (uncommitted). See "Implementation outcomes" at the end of this document for the finding-by-finding disposition, and verify with `make test` (hermetic), `make test-integration` (real Postgres), and `go vet ./...` — all green, including `-race`.

## TL;DR

The core is in good shape: the pipeline/wiki/diff/mapper packages are well-factored, well-tested (76–100% coverage), and the security thinking around the LLM agent is genuinely above average. The problems cluster in three places:

1. **The service layer is not ready for a team.** The HTTP API has zero authentication, workspace lookup isn't org-scoped, and one SQL statement writes across every tenant in the database.
2. **Cost correctness is leaky on the default path.** The analyze step's output is discarded, budgets aren't enforced per-call on the API runner, unknown model IDs silently disable budget checks, and failed imports never ledger the money already spent.
3. **Comments describe controls that don't exist.** Env isolation for the CLI child, model escalation, plan quarantine, cascade regeneration, date stamping, `--full`, `--with-worker` — all documented as working mechanisms, all no-ops. Treat comments in this repo as design notes until each is reconciled.

Nothing here is architecturally fatal. The recommended path is: fix tenancy/auth before any teammate touches a shared deployment, then the cost-correctness cluster, then reconcile or delete the dead features.

---

## What's working well

Worth stating explicitly, because these should be preserved through any refactor:

- **Migration runner** (`internal/store/migrate.go`) — embedded migrations, advisory-lock guarded, per-migration transactions, refuses startup on version skew in both directions, and a test that prevents forgetting to bump the schema version constant. A real 4-way concurrency test proves it (`store_integration_test.go:81`).
- **Prompt-injection defense** is layered and deliberate: sources framed as untrusted data (`internal/jobs/prompts.go:58`, `context.go:49`), CLI runner passes `--setting-sources ""` and `--strict-mcp-config` so ingested repos' `CLAUDE.md`/`.claude` can't load (`internal/agent/claude.go:113`), `Bash`/`WebSearch`/`WebFetch`/`Task` denied (`claude.go:22`), and a test asserting `--dangerously-skip-permissions` is never emitted. The default API runner removes the filesystem entirely.
- **Agent output path validation** — `wiki.checkPath` (`internal/wiki/validate.go:127`) rejects absolute paths, `..`, wrong depth, wrong type dirs, non-`.md`; `collectPages` independently re-checks with `EvalSymlinks` + `filepath.Rel` (`internal/jobs/collect.go:41`).
- **Secrets hygiene** — `Secrets` kept off the mapstructure-decoded struct so `%+v` can't leak it (`internal/config/config.go:31`), `_FILE` indirection for container secrets, `Database.Redacted()` for logs, `sslmode=require` by default.
- **Transaction discipline** — `Import` is a single transaction with `defer tx.Rollback(context.WithoutCancel(ctx))`, used consistently.
- **Test harnesses** — the compiled `fakeclaude` binary asserting real argv/cwd/timeouts, and `api_test.go` asserting on actual SDK wire requests, are both excellent patterns.
- **Design seams** — consumer-defined `jobs.Store` interface; the `Runner` abstraction with `WritesFiles` as the single branch point; the API runner returning pages as data (eliminating a whole vulnerability class); connector `Native any` with typed accessors.

---

## Critical

### C1. The HTTP API has no authentication

`internal/api/api.go:31-63` — middleware is `RequestID`, `Recoverer`, `Timeout(30s)`. Nothing else. Every workspace, page body, search index, and gap report is readable by anyone who can reach the port. The schema ships a complete authn/authz model — `users`, `sessions`, `org_members`, `tokens` with `token_hash`/`scopes` (`001_initial.sql:23-323`) — with zero Go behind it. `internal/auth` and `internal/crypto` are empty directories; `Secrets.SessionSecret` and `MasterKey` are loaded (`internal/config/secrets.go:23-26`) and never referenced.

**Fix:** before team use, add at minimum bearer-token auth against the existing `tokens` table (hash comparison, scope check per route) as chi middleware, plus org membership resolution. The schema already anticipates exactly this; it's wiring, not design work.

### C2. Workspace resolution is not org-scoped — cross-tenant disclosure by slug collision

`internal/api/api.go:306-309`:

```sql
SELECT id FROM workspaces WHERE slug = $1 OR id::text = $1 LIMIT 1
```

Slug uniqueness is per-org (`UNIQUE (org_id, slug)`, `001_initial.sql:94`). Two orgs each with a `docs` workspace → `LIMIT 1` with no `ORDER BY` returns an arbitrary row; requests for one org's wiki serve another's. Also, `resolve()` maps *every* error to 404 (`api.go:310-312`), so a dead database reads as "workspace not found".

**Fix:** scope the query by the authenticated org (depends on C1); distinguish `pgx.ErrNoRows` (404) from other errors (500 via `fail()`).

### C3. `resolveLinks`' second UPDATE has no wiki scope

`internal/store/jobstore.go:351-357` — the "unresolve dead links" statement:

```sql
UPDATE page_links l SET to_page_id = NULL, resolved = false
WHERE l.resolved AND NOT EXISTS (SELECT 1 FROM pages t WHERE t.id = l.to_page_id AND t.deleted_at IS NULL)
```

No `wiki_id` predicate. Every import scans, locks, and potentially rewrites `page_links` for **every wiki in the database**, inside the import transaction. Row results happen to be correct, but it's a cross-tenant lock-contention vector and a scaling cliff.

**Fix:** constrain via `l.from_page_id IN (SELECT id FROM pages WHERE wiki_id = $1)` like the first statement in the same function.

### C4. The `claude` CLI child inherits every secret in the environment

`internal/agent/claude.go:29-32` documents: *"The sandbox passes only ANTHROPIC_API_KEY and the minimum needed to run; connector credentials never appear here."* But `factory.go:26` calls `NewClaudeRunner(cfg.Agent.Binary)` which leaves `Env` nil, and `claude.go:61` only sets `cmd.Env` when `Env != nil` — so the child inherits the full parent environment: `KILN_MASTER_KEY`, `KILN_SESSION_SECRET`, DB password, everything. The agent processes untrusted source content; its process should not hold your master key. No test covers `Env`.

**Fix:** build an explicit minimal env in `factory.go` (`ANTHROPIC_API_KEY`, `PATH`, `HOME`/`TMPDIR` as needed) and add a `fakeclaude` test asserting `KILN_*` variables are absent from the child env.

### C5. Page identity split: pipeline dedupes by path, database dedupes by slug

- DB identity: `CREATE UNIQUE INDEX pages_wiki_slug_live_idx ON pages (wiki_id, slug)` (`001_initial.sql:161`); `upsertPage` conflicts on it and rewrites `path` in place (`jobstore.go:298-300`).
- Pipeline identity: `mergePages`, `countChanges`, `ValidateBatch` all key on `Path` (`collect.go:150-156`, `validate.go:188`). `Slug` is just the basename (`wiki/page.go:127`).

Consequences:
1. A run producing `entities/foo.md` and `concepts/foo.md` passes validation; the second upsert silently overwrites the first row. The index (built from the in-memory set) lists a page the store doesn't have.
2. Cascade deletion keys on paths (`files_written`; `softDeletePages` matches `path = ANY($2)`, `jobstore.go:373`). A page whose type changes moves directories; the upsert rewrites `path`, and the old source record's `files_written` now names a path no row has — soft-delete silently matches nothing.
3. The in-memory test store keys by path, so **no pipeline test can ever catch this class** (see A3).

**Fix:** pick one identity. Simplest: enforce slug uniqueness at validation time (`ValidateBatch` already has `KnownSlugs` — add a duplicate-slug violation across the batch and against existing pages at different paths), and key `files_written`/soft-deletion on slug rather than path.

---

## High

### H1. The analyze step's output is discarded — pure spend on the default runner

`internal/jobs/pipeline.go:317-338` runs `StepAnalyze`, accumulates cost/turns, and never reads `analyzeRes.Analysis` — the field (`api.go:233`) has zero consumers repo-wide. The CLI runner recovers implicitly via `--resume <session>`, but `APIRunner.Run` ignores `req.SessionID` entirely: analyze and generate are two independent stateless calls. Yet `generatePrompt` says *"Write the wiki pages you planned"* (`prompts.go:143`) — on the default runner (`factory.go:17`), that plan is not in context. Every analyze call on the API path is billed and thrown away.

**Fix:** feed `analyzeRes.Analysis` (serialized) into the generate prompt for the API runner, or skip analyze entirely when the runner is stateless. Add a test asserting analysis content reaches the generate request.

### H2. Budget enforcement has three holes

1. **Per-unit only:** the `spent >= RunUSD` check runs once per unit at the top of the loop (`pipeline.go:163`). A unit makes up to 4 model calls (analyze+generate × retries) with no check between; the final unit can overshoot arbitrarily.
2. **Unknown model = no budget:** `EstimateCostUSD` returns 0 for unrecognized model IDs (`pricing.go:47-52`); the comment says enforcement treats zero as "unknown, not free", but nothing does — `KnownModel` has no production callers. One typo'd or newer-than-the-table model ID silently disables run budgets.
3. **`BudgetUSD` unenforced on the API runner:** only the CLI path maps it to `--max-budget-usd` (`claude.go:128`); `APIRunner.Run` never reads it.

**Fix:** check budget between analyze and generate and between retries; fail fast at pipeline start if `!KnownModel(p.Model)` and a budget is set; have `APIRunner` compare estimated cost against `req.BudgetUSD` (even post-hoc per call is better than nothing). Also note `buildResult` prices against the *requested* model while reporting the *served* model (`api.go:193`) — price the served one.

### H3. Failed imports never ledger money already spent

`pipeline.go:246-248` returns early when `Store.Import` fails, so `RecordRun` (`pipeline.go:253`) never executes: billed agent calls produce no `runs` row and no spend-ledger entry, and budget windows undercount forever after. `Import` and `RecordRun` are also two separate transactions, so a crash between them loses run history for committed pages.

**Fix:** record the run (with a failed status) even when import fails — e.g. `defer`-style recording, or fold `RecordRun` into the `Import` transaction (it's the same store, same connection pool; one transaction is simpler and removes the crash window).

### H4. `kiln build` is uncancellable and doesn't check cancellation

- `main.go:15` uses `Execute()`, not `ExecuteContext()`, so every command except `serve` runs on `context.Background()`. Ctrl-C hard-kills mid-pipeline: the `defer os.RemoveAll(scratch/staging)` cleanups (`build.go:159,215`) never run, and `admin migrate` can't be interrupted while blocked on the advisory lock.
- The unit loop (`pipeline.go:162-208`) never checks `ctx.Err()`. On cancellation, every remaining unit runs, fails, gets recorded `StatusFailed`, and then `Import` is called with a dead context — **losing the units that succeeded**. A cancelled run looks like a total failure.

**Fix:** `signal.NotifyContext` in `main.go` + `ExecuteContext`; check `ctx.Err()` at the top of the unit loop and break (not fail) so completed work still imports — the "stop scheduling rather than failing" pattern already used for budget exhaustion is exactly right here too.

### H5. The incremental promise doesn't hold: arch overview regenerates every run

`build.go:205` always sends `ChangeSet{FullRebuild: true}`; `Route` then marks everything including `ArchOverview` (`route.go:68-76`). `filterUnchanged` does rescue module/doc units by hash — but `arch:overview` has no unit in the map, so `!ok` → always dirty (`pipeline.go:270`). Every `kiln build` on an untouched repo spends one analyze+generate pair on the overview. `WorkspaceMap.Hash` — documented at `repomap/scan.go:603` as the thing that "dirties the architecture synthesis" — is computed and never read. Meanwhile `TestBuildNoChangesCostsNothing` passes only because it doesn't use `FullRebuild`.

Related dead surface: the `--full` flag (`build.go:68`) is declared and never read, and `diff.HashDiff` (the entire non-git change-detection path) has no production caller.

**Fix:** persist `WorkspaceMap.Hash` in the sources table under an `arch:overview` key and gate it in `filterUnchanged` like any other unit; wire `--full` to skip `filterUnchanged`; add a CLI-path test that an unchanged repo makes zero agent calls.

### H6. Concurrent builds on one workspace are unserialized

Nothing locks a workspace during a run. Two concurrent builds both read the same baseline, both generate (both spend money), both import; last writer wins. Compounding races inside the store:

- `ensureWiki` is check-then-insert (`jobstore.go:267-284`); `wikis.workspace_id` is UNIQUE, so a concurrent first-build hard-fails `Import` with a unique violation instead of retrying.
- `writeArtifacts` iterates a two-key Go map (`jobstore.go:219`), randomizing upsert order — a lock-order-inversion deadlock candidate between concurrent imports.

**Fix:** take `pg_advisory_xact_lock(hashtext(workspace_id))` at the top of `Import` (serializes the cheap part), use `INSERT … ON CONFLICT (workspace_id) DO UPDATE SET workspace_id = EXCLUDED.workspace_id RETURNING id` in `ensureWiki`, and write artifacts in fixed order. Full run-level serialization can wait for the job queue.

---

## Medium

### M1. Documented features that are no-ops (reconcile or delete)

| Feature | Where | Reality |
|---|---|---|
| Model escalation on final retry | `pipeline.go:403-408` | Both branches `return p.Model`. `FallbackModel` and `agent.EscalationModel` are dead. |
| Cascade regeneration | `diff/cascade.go:21-23` | `RegeneratePages`/`DeleteBlobs` computed, never consumed (`pipeline.go:214-242` uses only `DeletePages`/`DropSources`). Surviving pages keep prose about deleted sources forever. |
| Plan quarantine | `wiki/validate.go:32` | `ValidateOptions.Planned` never populated — nothing constrains the agent to the approved plan. |
| Date stamping | `pipeline.go:372` | `RequireDates: false` "when the caller stamps them" — no caller does. `wiki.Today()` has zero production callers. "Recently Updated" in the index is always empty on the API path. |
| Cacheable context (CLI) | `claude.go:110-166` | `req.CacheableContext` (the repo map!) never referenced in `BuildArgs` — the CLI agent never sees the map summary. Real data loss, not just a missed cache. |
| Permission-denial surfacing | `agent.go:100-106` | Parsed, never consumed. `Agent.WarnTurns` likewise never read. |
| `serve --with-worker` | `serve.go:65-70` | Prints a line. No worker command exists despite `main.go:5` and README describing one. Honest comment, misleading UX. |
| Tokens in run summary | `jobs/store.go:86` | `Summary.Tokens` never populated; `runs.tokens` always 0. `Summary.Err` never set. |

**Fix:** for each — implement it, or delete the field/flag/comment. The dangerous state is the current one, where a reader (or a future you) assumes the control exists. Suggested implements: dates (trivial — stamp in `pagesFrom`, preserve `Created` for existing slugs), cascade regen (append `RegeneratePages` to the dirty set), plan quarantine (pass planned paths from the analyze schema once H1 wires it). Suggested deletes: `modelFor` escalation until you actually want it, `--with-worker` until the queue lands.

### M2. Doc-section units are unreachable

`docmap.unitsFor` emits `doc:<key>#<section>` units (`docmap.go:145-159`), but the router only registers whole-doc routes (`build.go:275-277`) and the FullRebuild branch iterates `DocPaths` only (`route.go:68-76`). Section units are never routed, never generated — dead weight in every map merge. Related: `docmap.Mapper.Map` never populates `Doc.Text` despite its comment, so the generic `SourceSet` path always yields zero sections.

**Fix:** either route sections (register `doc:key#section` keys against the parent path) or stop emitting them until section-level generation is real.

### M3. API layer: no pagination, loads full bodies, reaches around its own store

- `handlePages` calls `Store.LoadPages`, which selects **every page body** (`jobstore.go:69`), then discards bodies to build summaries. `handlePage` (`api.go:177-196`) loads the whole workspace and linear-scans for one page. No `LIMIT` on `/gaps`; search is hardcoded `LIMIT 50` with no cursor.
- `wikis.revision` exists precisely for cache busting and is exposed on the workspace summary, but no handler emits `ETag`/`If-None-Match` or `Cache-Control`.
- No CORS middleware — the planned separate-origin Next.js frontend cannot call this API at all.
- `Server` holds both `*store.JobStore` and `*store.DB` and half the handlers write raw SQL (`handleWorkspaces`, `handleSearch`, `handleGaps`, `resolve`) while half go through the store. This split is also why every API test needs Postgres.

**Fix:** add `LoadPageSummaries` (no body) and `LoadPage(slug)` store methods; paginate `/pages` and `/gaps`; emit `ETag: W/"rev-N"`; add CORS when the frontend lands; move the raw SQL into the store and consider a narrow `api.Store` interface so handlers can be tested against a fake.

### M4. Parse failures become misleading retry prompts

`collect.go:55-62`: a scratch file that fails `wiki.ParsePage` is replaced with an empty `wiki.Page` and the real error (`ErrNoFrontmatter`, "unterminated frontmatter") is dropped. The retry prompt then tells the model "body is empty / missing heading" instead of "your frontmatter fence is broken" — burning a retry on a mis-diagnosis.

**Fix:** carry the parse error into a violation with the actual message.

### M5. Transient API failures fail the unit immediately

`api.go:115` relies on SDK-default retries only; a 429/529 surfaces as a unit failure, and the pipeline's retry loop fires only on *validation* failures (`pipeline.go:349-352`). A mid-stream network blip discards accumulated content (`api.go:167-174`) and costs the whole unit.

**Fix:** distinguish retryable API errors (429/5xx/stream interruption) and retry them within the attempt with backoff, separate from validation retries.

### M6. Migration lock edge cases

`pg_advisory_lock` with no timeout (`migrate.go:53`) wedges every later invocation behind a hung migration, silently. If the deferred `pg_advisory_unlock` errors, `conn.Release()` returns a session holding the lock to the pool, leaking it for the connection's lifetime.

**Fix:** `SET lock_timeout` / `context.WithTimeout` around acquisition with a clear error; on unlock failure, `conn.Conn().Close(ctx)` instead of releasing to the pool.

### M7. Sub-partitioned module parents go permanently stale

`subPartition` (`repomap/scan.go:276`) creates child modules whose files also count into the parent's `Hash`/`LOC`, but `longestPrefix` routing (`route.go:145`) attributes changes only to the deepest module. The parent's hash changes, the parent is never routed, its page describes an old state forever. (Masked today by H5's always-regenerate behavior for arch, but real for module parents.)

**Fix:** either exclude child-owned files from the parent's hash, or route the parent whenever a child is routed.

---

## Low

- `sanitize` (`collect.go:260`) — `strings.NewReplacer` is single-pass; the `".." → "_"` rule is unreachable after `"/" → "_"`, and distinct keys can collide (`module:a/b` vs `module:a_b`) → scratch-dir/session collisions across units. Use a hash suffix or stricter mapping. Also `collect.go:264`: stray `var _ = diff.Key("")`.
- `truncatePreservingArch` (`collect.go:206`) shadows the builtin `cap`.
- Log-append logic exists twice and disagrees: `wiki.AppendLogEntry` (`log.go:46`, zero production callers, tested) vs the SQL `body || E'\n' || $3` (`jobstore.go:240-247`, untested separators/header). Delete one.
- Duplicated analysis schema: `jobs.AnalysisSchema` (`prompts.go:15`) vs `agent.AnalysisSchemaJSON` (`schema.go:83`); the `jobs` copy lacks `additionalProperties: false`. Single-source it.
- `mapper.SourceSet`/`Mapper` interface is bypassed by every production caller (both go through `Native` type assertions). Either commit to the interface or remove it.
- Tested-but-unused exported surface: `diff.ParseKey`, `diff.StaleAdjacent`, `diff.StripSource`, `wiki.PruneDeadLinks`, `wiki.FilterRelated`, `Router.Cosmetic` (never set by `routerFor`). Prune or wire.
- `store.JobStore` is misnamed — it's the wiki persistence layer, not a job store. Rename before the real job queue lands, or the collision will hurt.
- `ui.go:31` uses a manual `req.URL.Path[:5]` slice instead of `strings.HasPrefix`; only `GET /` is registered so `POST /` 405s instead of serving the SPA shell.
- `Serve` sets `ReadHeaderTimeout` only — add `ReadTimeout`/`WriteTimeout`/`IdleTimeout`.
- No upload/extract size limits: `PassthroughExtractor` does a bare `os.ReadFile` (`extract.go:172`) and full text is retained in memory; `pandoc` runs on untrusted documents without `--sandbox`. Fine for CLI-only today; must be capped before uploads become API-driven.
- Connectors accept any absolute local path (`git.go:40`, `upload.go:56`) — safe while only the CLI supplies it; becomes LFI the moment paths are API/DB-driven. Leave a guard-rail comment or add an allowlist now.
- `generateUnit` re-runs analyze on every retry (`pipeline.go:316-328`) even when only generation failed — doubling correction cost.

---

## Architecture assessment (for team use)

**A1. The gap between documented and actual architecture is the meta-issue.** README and `main.go` describe serve/worker roles against a queue; there is no worker, no jobs table, no queue semantics, and `config.Worker{Concurrency, SweepInterval, SweepJitter}` is validated and never read. The same pattern repeats at micro scale (M1). Before adding features, do a reconciliation pass: every comment describing a control either gets an implementation with a test, or gets deleted. This is unusually important here because the product's own premise is documentation that stays true to its sources.

**A2. The job queue is the next real architectural step.** When it lands: a `jobs` table with `SELECT … FOR UPDATE SKIP LOCKED`, attempt counters, and per-workspace serialization solves H6 for free. `Serve`'s current signature (single error return, no place to join a worker pool) will need reshaping — worth doing when `ExecuteContext` lands (H4).

**A3. The memstore/pgstore drift undermines the pipeline's test story.** All 29 pipeline tests run against `memStore`, which keys pages by path (Postgres: slug), hard-deletes (Postgres: tombstones), ignores artifacts, and can't fail mid-import. That's why C5 is invisible to the suite. Options, cheapest first: (a) make `memStore` enforce slug uniqueness and tombstones so it's behavior-faithful; (b) add a small contract-test suite run against both implementations (the integration harness already exists); (c) both. Also lift `LoadArtifact`/`EnsureWorkspace` onto the `jobs.Store` interface so `api.Server` can depend on an interface instead of concrete `*store.JobStore` + `*store.DB`.

**A4. No CI.** `.github/` doesn't exist. `make test` skips everything Postgres-gated, so `internal/store` (6% hermetic coverage), `internal/api` (18%), and `cmd/kiln` (0%) are effectively unreviewed by machines. A minimal GitHub Actions workflow — `make test` + `go vet` on every push, `make test-integration` with a Postgres service container — would have caught several items above at commit time.

**A5. Security posture summary.** The agent sandbox thinking is strong (see "working well"); the service perimeter is the weak half. Ordered by exposure: C1 (no auth) → C4 (secret env leak into the process that reads untrusted input) → C2/C3 (tenancy). None are hard fixes; all should precede any shared deployment.

---

## Suggested fix roadmap

**P0 — before any shared/team deployment**
C4 (env isolation — smallest diff, do first), C3 (one-line SQL scope), C2 (org-scoped resolve + error split), C1 (token auth middleware over the existing `tokens` table).

**P1 — cost & data correctness**
H3 (ledger failed runs), H2 (budget holes), H1 (feed analysis into generate or drop the step), H5 (arch-overview hash gate + `--full`), C5 (slug/path identity), H4 (cancellation), M4 (parse-error violations).

**P2 — truth reconciliation**
M1 table (implement or delete each), M2 (doc sections), M7 (parent hashes), Low-list dead code pruning, rename `store.JobStore`.

**P3 — service hardening & platform**
M3 (pagination/ETag/CORS/store methods), M5 (retryable API errors), M6 (lock timeouts), H6 leftovers (advisory lock in Import), A4 (CI), A3 (contract tests), then the job queue (A2).

Each P0/P1 item is independently shippable; happy to take any subset next.
---[37m[39;49;00m
[37m[39;49;00m
[37m## Implementation outcomes (2026-07-25)[39;49;00m[37m[39;49;00m
[37m[39;49;00m
All[37m [39;49;00mfour[37m [39;49;00mphases[37m [39;49;00mimplemented.[37m [39;49;00mSuite[37m [39;49;00mgreen:[37m [39;49;00mhermetic[37m [39;49;00m([04m[91m`[39;49;00mmake[37m [39;49;00mtest[04m[91m`[39;49;00m),[37m [39;49;00mrace[37m [39;49;00m([04m[91m`[39;49;00mgo[37m [39;49;00mtest[37m [39;49;00m-race[37m [39;49;00m./...[04m[91m`[39;49;00m),[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mintegration[37m [39;49;00m([04m[91m`[39;49;00mmake[37m [39;49;00mtest-integration[04m[91m`[39;49;00m[37m [39;49;00magainst[37m [39;49;00mPostgres[37m [39;49;00m[34m16[39;49;00m[37m [39;49;00m[35min[39;49;00m[37m [39;49;00mDocker).[37m[39;49;00m
[37m[39;49;00m
[37m### P0 — security & tenancy[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**C1[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mbearer-token[37m [39;49;00mauth:[37m [39;49;00mnew[37m [39;49;00m[04m[91m`[39;49;00minternal/auth[04m[91m`[39;49;00m[37m [39;49;00mpackage[37m [39;49;00m(SHA-[34m256[39;49;00m-hashed[37m [39;49;00mtokens[37m [39;49;00magainst[37m [39;49;00mthe[37m [39;49;00mexisting[37m [39;49;00m[04m[91m`[39;49;00mtokens[04m[91m`[39;49;00m[37m [39;49;00mtable,[37m [39;49;00mscope[37m [39;49;00mchecks,[37m [39;49;00morg[37m [39;49;00mmembership[37m [39;49;00mresolved[37m [39;49;00mper[37m [39;49;00mquery),[37m [39;49;00menforced[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mmiddleware[37m [39;49;00mon[37m [39;49;00mall[37m [39;49;00m[04m[91m`[39;49;00m/api/v1[04m[91m`[39;49;00m[37m [39;49;00mroutes[37m [39;49;00mexcept[37m [39;49;00m[04m[91m`[39;49;00m/version[04m[91m`[39;49;00m.[37m [39;49;00m[04m[91m`[39;49;00mkiln[37m [39;49;00madmin[37m [39;49;00mtoken[37m [39;49;00mcreate|revoke[04m[91m`[39;49;00m[37m [39;49;00mmints/revokes[37m [39;49;00mtokens[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mbootstraps[37m [39;49;00muser[37m [39;49;00m+[37m [39;49;00morg[37m [39;49;00mmembership.[37m [39;49;00mThe[37m [39;49;00membedded[37m [39;49;00mUI[37m [39;49;00mstores[37m [39;49;00ma[37m [39;49;00mtoken[37m [39;49;00m[35min[39;49;00m[37m [39;49;00mlocalStorage[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mshows[37m [39;49;00ma[37m [39;49;00m[36msign[39;49;00m-[35min[39;49;00m[37m [39;49;00mform[37m [39;49;00mon[37m [39;49;00m[34m401.[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mauth.mode[37m [39;49;00m=[37m [39;49;00m[33m"[39;49;00m[33mtoken[39;49;00m[33m"[39;49;00m[04m[91m`[39;49;00m[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mthe[37m [39;49;00mdefault;[37m [39;49;00m[04m[91m`[39;49;00m[33m"[39;49;00m[33mnone[39;49;00m[33m"[39;49;00m[04m[91m`[39;49;00m[37m [39;49;00mmust[37m [39;49;00mbe[37m [39;49;00mopted[37m [39;49;00minto[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mlogs[37m [39;49;00ma[37m [39;49;00mloud[37m [39;49;00mwarning.[37m[39;49;00m
-[37m [39;49;00m**C2[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mworkspace[37m [39;49;00mresolution[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mbounded[37m [39;49;00mto[37m [39;49;00morgs[37m [39;49;00mthe[37m [39;49;00mcaller[37m [39;49;00mcan[37m [39;49;00msee,[37m [39;49;00m[04m[91m`[39;49;00mORDER[37m [39;49;00mBY[37m [39;49;00mcreated_at[04m[91m`[39;49;00m[37m [39;49;00m[34mfor[39;49;00m[37m [39;49;00mdeterminism,[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mpgx.ErrNoRows[04m[91m`[39;49;00m[37m [39;49;00m([34m404[39;49;00m)[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mdistinguished[37m [39;49;00mfrom[37m [39;49;00moutages[37m [39;49;00m([34m500[39;49;00m).[37m[39;49;00m
-[37m [39;49;00m**C3[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mthe[37m [39;49;00mdead-link[37m [39;49;00mUPDATE[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mscoped[37m [39;49;00mto[37m [39;49;00mthe[37m [39;49;00mimporting[37m [39;49;00mwiki[33m'[39;49;00m[33ms own links.[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**C4[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00magent.New[04m[91m`[39;49;00m[37m [39;49;00malways[37m [39;49;00msets[37m [39;49;00man[37m [39;49;00mexplicit[37m [39;49;00m[04m[91m`[39;49;00mMinimalChildEnv[04m[91m`[39;49;00m[37m [39;49;00m(PATH/HOME/TMPDIR/TERM/LANG[37m [39;49;00m+[37m [39;49;00m[04m[91m`[39;49;00mANTHROPIC_API_KEY[04m[91m`[39;49;00m)[37m [39;49;00mon[37m [39;49;00mthe[37m [39;49;00mCLI[37m [39;49;00mrunner;[37m [39;49;00ma[37m [39;49;00mfakeclaude[37m [39;49;00mtest[37m [39;49;00masserts[37m [39;49;00m[04m[91m`[39;49;00mKILN_*[04m[91m`[39;49;00m[37m [39;49;00msecrets[37m [39;49;00mnever[37m [39;49;00mreach[37m [39;49;00mthe[37m [39;49;00mchild.[37m[39;49;00m
-[37m [39;49;00m**C5[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mValidateBatch[04m[91m`[39;49;00m[37m [39;49;00mrejects[37m [39;49;00mduplicate[37m [39;49;00mslugs[37m [39;49;00mwithin[37m [39;49;00ma[37m [39;49;00mbatch[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mcollisions[37m [39;49;00magainst[37m [39;49;00mlive[37m [39;49;00mpages[37m [39;49;00mat[37m [39;49;00mother[37m [39;49;00mpaths;[37m [39;49;00m[04m[91m`[39;49;00msoftDeletePages[04m[91m`[39;49;00m[37m [39;49;00mmatches[37m [39;49;00mby[37m [39;49;00mslug[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mwell[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mpath;[37m [39;49;00mthe[37m [39;49;00mloop[37m [39;49;00mthreads[37m [39;49;00mnewly[37m [39;49;00mwritten[37m [39;49;00mslugs[37m [39;49;00mso[37m [39;49;00mlater[37m [39;49;00munits[37m [39;49;00msee[37m [39;49;00mearlier[37m [39;49;00munits[33m'[39;49;00m[33m pages.[39;49;00m[37m[39;49;00m
[37m[39;49;00m
[37m### P1 — cost & data correctness[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**H1[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00manalyze[37m [39;49;00mruns[37m [39;49;00monce[37m [39;49;00mper[37m [39;49;00munit[37m [39;49;00m([35mnot[39;49;00m[37m [39;49;00mper[37m [39;49;00mretry);[37m [39;49;00mits[37m [39;49;00mplan[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mserialized[37m [39;49;00minto[37m [39;49;00mthe[37m [39;49;00mgenerate[37m [39;49;00mprompt[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mits[37m [39;49;00mpage[37m [39;49;00mlist[37m [39;49;00mbecomes[37m [39;49;00mthe[37m [39;49;00m[04m[91m`[39;49;00mPlanned[04m[91m`[39;49;00m[37m [39;49;00mquarantine[37m [39;49;00mset.[37m [39;49;00mTests[37m [39;49;00m[36massert[39;49;00m[37m [39;49;00mthe[37m [39;49;00mplan[37m [39;49;00mreaches[37m [39;49;00mgeneration[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mthat[37m [39;49;00munplanned[37m [39;49;00mpages[37m [39;49;00mare[37m [39;49;00mrejected.[37m[39;49;00m
-[37m [39;49;00m**H2[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mbudget[37m [39;49;00mchecked[37m [39;49;00mbetween[37m [39;49;00manalyze/generate[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mbetween[37m [39;49;00mretries;[37m [39;49;00m[04m[91m`[39;49;00mBuild[04m[91m`[39;49;00m[37m [39;49;00mrefuses[37m [39;49;00mto[37m [39;49;00mstart[37m [39;49;00ma[37m [39;49;00mbudgeted[37m [39;49;00mrun[37m [39;49;00mon[37m [39;49;00mthe[37m [39;49;00mAPI[37m [39;49;00mrunner[37m [39;49;00mwhen[37m [39;49;00many[37m [39;49;00mconfigured[37m [39;49;00mmodel[37m [39;49;00mhas[37m [39;49;00mno[37m [39;49;00mpricing[37m [39;49;00mentry;[37m [39;49;00m[04m[91m`[39;49;00mAPIRunner[04m[91m`[39;49;00m[37m [39;49;00mprices[37m [39;49;00mthe[37m [39;49;00mserved[37m [39;49;00mmodel[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mflags[37m [39;49;00m[04m[91m`[39;49;00mResult.OverBudget[04m[91m`[39;49;00m[37m [39;49;00mpost-hoc.[37m[39;49;00m
-[37m [39;49;00m**H3[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00ma[37m [39;49;00mfailed[37m [39;49;00mimport[37m [39;49;00mstill[37m [39;49;00mrecords[37m [39;49;00mthe[37m [39;49;00mrun[37m [39;49;00m(status[37m [39;49;00m[04m[91m`[39;49;00mfailed[04m[91m`[39;49;00m,[37m [39;49;00mcost,[37m [39;49;00merror);[37m [39;49;00mimport[37m [39;49;00m+[37m [39;49;00mrun[37m [39;49;00mrecord[37m [39;49;00mrun[37m [39;49;00mon[37m [39;49;00m[04m[91m`[39;49;00mcontext.WithoutCancel[04m[91m`[39;49;00m.[37m[39;49;00m
-[37m [39;49;00m**H4[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mmain.go[04m[91m`[39;49;00m[37m [39;49;00muses[37m [39;49;00m[04m[91m`[39;49;00m[34msignal[39;49;00m.NotifyContext[04m[91m`[39;49;00m[37m [39;49;00m+[37m [39;49;00m[04m[91m`[39;49;00mExecuteContext[04m[91m`[39;49;00m;[37m [39;49;00mthe[37m [39;49;00munit[37m [39;49;00mloop[37m [39;49;00mchecks[37m [39;49;00m[04m[91m`[39;49;00mctx.Err()[04m[91m`[39;49;00m[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mbreaks[37m [39;49;00m(status[37m [39;49;00m[04m[91m`[39;49;00mcanceled[04m[91m`[39;49;00m),[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mcompleted[37m [39;49;00munits[37m [39;49;00mstill[37m [39;49;00mimport.[37m [39;49;00mTemp-dir[37m [39;49;00mdefers[37m [39;49;00mnow[37m [39;49;00mrun[37m [39;49;00mon[37m [39;49;00mCtrl-C.[37m[39;49;00m
-[37m [39;49;00m**H5[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00march:overview[04m[91m`[39;49;00m[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mgated[37m [39;49;00mby[37m [39;49;00mthe[37m [39;49;00mwhole-map[37m [39;49;00m[36mhash[39;49;00m[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mrecorded[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00ma[37m [39;49;00msource;[37m [39;49;00ma[37m [39;49;00mrepeat[37m [39;49;00m[04m[91m`[39;49;00mkiln[37m [39;49;00mbuild[04m[91m`[39;49;00m[37m [39;49;00mover[37m [39;49;00munchanged[37m [39;49;00msources[37m [39;49;00mmakes[37m [39;49;00mzero[37m [39;49;00magent[37m [39;49;00mcalls[37m [39;49;00m(tested).[37m [39;49;00m[04m[91m`[39;49;00m--full[04m[91m`[39;49;00m[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mwired[37m [39;49;00mto[37m [39;49;00m[04m[91m`[39;49;00mBuildRequest.Force[04m[91m`[39;49;00m.[37m[39;49;00m
-[37m [39;49;00m**H6[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mper-workspace[37m [39;49;00m[04m[91m`[39;49;00mpg_advisory_xact_lock[04m[91m`[39;49;00m[37m [39;49;00mserializes[37m [39;49;00mimports;[37m [39;49;00m[04m[91m`[39;49;00mensureWiki[04m[91m`[39;49;00m[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00ma[37m [39;49;00msingle[37m [39;49;00mupsert;[37m [39;49;00martifacts[37m [39;49;00mwrite[37m [39;49;00m[35min[39;49;00m[37m [39;49;00mfixed[37m [39;49;00morder.[37m[39;49;00m
-[37m [39;49;00m**M4[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mparse[37m [39;49;00mfailures[37m [39;49;00msurface[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mviolations[37m [39;49;00mcarrying[37m [39;49;00mthe[37m [39;49;00mreal[37m [39;49;00mparser[37m [39;49;00merror;[37m [39;49;00ma[37m [39;49;00mtest[37m [39;49;00masserts[37m [39;49;00mthe[37m [39;49;00mretry[37m [39;49;00mprompt[37m [39;49;00mnames[37m [39;49;00mthe[37m [39;49;00mbroken[37m [39;49;00mfrontmatter,[37m [39;49;00m[35mnot[39;49;00m[37m [39;49;00m[33m"[39;49;00m[33mbody is empty[39;49;00m[33m"[39;49;00m.[37m[39;49;00m
[37m[39;49;00m
[37m### P2 — truth reconciliation[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**Implemented**:[37m [39;49;00mmodel[37m [39;49;00mescalation[37m [39;49;00m([04m[91m`[39;49;00mmodelFor[04m[91m`[39;49;00m[37m [39;49;00mreturns[37m [39;49;00mthe[37m [39;49;00mfallback[37m [39;49;00mon[37m [39;49;00mthe[37m [39;49;00mfinal[37m [39;49;00mattempt),[37m [39;49;00mcascade[37m [39;49;00mregeneration[37m [39;49;00m([04m[91m`[39;49;00mRegeneratePages[04m[91m`[39;49;00m[37m [39;49;00mmapped[37m [39;49;00mback[37m [39;49;00mto[37m [39;49;00msurviving[37m [39;49;00msource[37m [39;49;00munits[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mappended[37m [39;49;00mto[37m [39;49;00mthe[37m [39;49;00mdirty[37m [39;49;00mset),[37m [39;49;00mplan[37m [39;49;00mquarantine[37m [39;49;00m(see[37m [39;49;00mH1),[37m [39;49;00mdate[37m [39;49;00mstamping[37m [39;49;00m([04m[91m`[39;49;00mUpdated[04m[91m`[39;49;00m[37m [39;49;00m=[37m [39;49;00mrun[37m [39;49;00mdate,[37m [39;49;00m[04m[91m`[39;49;00mCreated[04m[91m`[39;49;00m[37m [39;49;00mpreserved[37m [39;49;00mvia[37m [39;49;00m[04m[91m`[39;49;00mexistingCreated[04m[91m`[39;49;00m),[37m [39;49;00mCLI[37m [39;49;00m[04m[91m`[39;49;00mCacheableContext[04m[91m`[39;49;00m[37m [39;49;00m(folded[37m [39;49;00minto[37m [39;49;00m[04m[91m`[39;49;00m--append-system-prompt[04m[91m`[39;49;00m),[37m [39;49;00mpermission-denial[37m [39;49;00m+[37m [39;49;00m[04m[91m`[39;49;00mWarnTurns[04m[91m`[39;49;00m[37m [39;49;00m+[37m [39;49;00mover-budget[37m [39;49;00mlogging,[37m [39;49;00m[04m[91m`[39;49;00mSummary.Tokens[04m[91m`[39;49;00m/[04m[91m`[39;49;00mSummary.Err[04m[91m`[39;49;00m[37m [39;49;00mpopulated.[37m[39;49;00m
-[37m [39;49;00m**Deleted**:[37m [39;49;00m[04m[91m`[39;49;00m--with-worker[04m[91m`[39;49;00m[37m [39;49;00mflag,[37m [39;49;00m[04m[91m`[39;49;00mconfig.Worker[04m[91m`[39;49;00m[37m [39;49;00m(never[37m [39;49;00mread),[37m [39;49;00m[04m[91m`[39;49;00mHashDiff[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mParseKey[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mStaleAdjacent[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mStripSource[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mPruneDeadLinks[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mFilterRelated[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mToday[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mAppendLogEntry[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mChangeSet.Paths/Counts[04m[91m`[39;49;00m[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mplus[37m [39;49;00mtheir[37m [39;49;00mtests.[37m [39;49;00m[04m[91m`[39;49;00mmain.go[04m[91m`[39;49;00m/README[37m [39;49;00mno[37m [39;49;00mlonger[37m [39;49;00mdescribe[37m [39;49;00ma[37m [39;49;00mworker/queue[37m [39;49;00mthat[37m [39;49;00mdoesn[33m'[39;49;00m[33mt exist.[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**M2[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mdoc[37m [39;49;00msections[37m [39;49;00mare[37m [39;49;00mrouted:[37m [39;49;00m[04m[91m`[39;49;00mRouter.DocSections[04m[91m`[39;49;00m[37m [39;49;00mmarks[37m [39;49;00msection[37m [39;49;00mkeys[37m [39;49;00mwhenever[37m [39;49;00mtheir[37m [39;49;00mparent[37m [39;49;00mdocument[37m [39;49;00mroutes[37m [39;49;00m(both[37m [39;49;00mbranches),[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mdocmap.Map[04m[91m`[39;49;00m[37m [39;49;00mnow[37m [39;49;00mreads[37m [39;49;00mstaged[37m [39;49;00mtext[37m [39;49;00mso[37m [39;49;00msections[37m [39;49;00mform[37m [39;49;00mon[37m [39;49;00mthe[37m [39;49;00mgeneric[37m [39;49;00mpath[37m [39;49;00mtoo.[37m[39;49;00m
-[37m [39;49;00m**M7[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00ma[37m [39;49;00msub-partitioned[37m [39;49;00mparent[37m [39;49;00mre-hashes[37m [39;49;00mover[37m [39;49;00monly[37m [39;49;00mthe[37m [39;49;00mfiles[37m [39;49;00mits[37m [39;49;00mchildren[37m [39;49;00mdon[33m'[39;49;00m[33mt claim, so it can[39;49;00m[33m'[39;49;00mt[37m [39;49;00mgo[37m [39;49;00mstale-but-gated[37m [39;49;00m(tested).[37m[39;49;00m
-[37m [39;49;00m**Low[37m [39;49;00mlist**:[37m [39;49;00m[04m[91m`[39;49;00msanitize[04m[91m`[39;49;00m[37m [39;49;00mcollision-proofed[37m [39;49;00mwith[37m [39;49;00ma[37m [39;49;00m[36mhash[39;49;00m[37m [39;49;00msuffix,[37m [39;49;00m[04m[91m`[39;49;00mcap[04m[91m`[39;49;00m[37m [39;49;00mshadow[37m [39;49;00mrenamed,[37m [39;49;00manalysis[37m [39;49;00mschema[37m [39;49;00msingle-sourced[37m [39;49;00mfrom[37m [39;49;00m[04m[91m`[39;49;00magent.AnalysisSchemaText()[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mRouter.Cosmetic[04m[91m`[39;49;00m[37m [39;49;00mwired[37m [39;49;00mwith[37m [39;49;00ma[37m [39;49;00mdefault[37m [39;49;00mfilter,[37m [39;49;00m[04m[91m`[39;49;00mstore.JobStore[04m[91m`[39;49;00m[37m [39;49;00mrenamed[37m [39;49;00m[04m[91m`[39;49;00mWikiStore[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mui.go[04m[91m`[39;49;00m[37m [39;49;00muses[37m [39;49;00m[04m[91m`[39;49;00mstrings.HasPrefix[04m[91m`[39;49;00m,[37m [39;49;00mserver[37m [39;49;00mgains[37m [39;49;00mRead/Write/Idle[37m [39;49;00mtimeouts,[37m [39;49;00mpassthrough[37m [39;49;00mextraction[37m [39;49;00mcapped[37m [39;49;00mat[37m [39;49;00m[34m32[39;49;00m[37m [39;49;00mMiB,[37m [39;49;00mpandoc[37m [39;49;00mruns[37m [39;49;00m[04m[91m`[39;49;00m--sandbox[04m[91m`[39;49;00m,[37m [39;49;00mCLI[37m [39;49;00mrunner[37m [39;49;00mrequires[37m [39;49;00ma[37m [39;49;00mprompt,[37m [39;49;00mLFI[37m [39;49;00mguard-rail[37m [39;49;00mcomments[37m [39;49;00mon[37m [39;49;00mboth[37m [39;49;00mconnectors.[37m[39;49;00m
[37m[39;49;00m
[37m### P3 — service hardening & platform[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**M3[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mread[37m [39;49;00mpath[37m [39;49;00mrebuilt[37m [39;49;00mon[37m [39;49;00mnew[37m [39;49;00m[04m[91m`[39;49;00mWikiStore[04m[91m`[39;49;00m[37m [39;49;00mmethods[37m [39;49;00m([04m[91m`[39;49;00mListWorkspaces[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mResolveWorkspace[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mLoadPageSummaries[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mLoadPage[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mSearch[04m[91m`[39;49;00m,[37m [39;49;00m[04m[91m`[39;49;00mGaps[04m[91m`[39;49;00m):[37m [39;49;00mno[37m [39;49;00mbodies[37m [39;49;00mloaded[37m [39;49;00m[34mfor[39;49;00m[37m [39;49;00mlistings,[37m [39;49;00msingle-page[37m [39;49;00mlookup[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mone[37m [39;49;00mquery,[37m [39;49;00mall[37m [39;49;00mlist[37m [39;49;00mendpoints[37m [39;49;00mpaginate[37m [39;49;00m([04m[91m`[39;49;00mlimit[04m[91m`[39;49;00m/[04m[91m`[39;49;00moffset[04m[91m`[39;49;00m[37m [39;49;00mwith[37m [39;49;00mceilings),[37m [39;49;00m[04m[91m`[39;49;00mETag:[37m [39;49;00mW/[33m"[39;49;00m[33m<ws>-<rev>[39;49;00m[33m"[39;49;00m[04m[91m`[39;49;00m[37m [39;49;00m+[37m [39;49;00m[04m[91m`[39;49;00mIf-None-Match[04m[91m`[39;49;00m[37m [39;49;00m[04m[91m→[39;49;00m[37m [39;49;00m[34m304[39;49;00m[37m [39;49;00mon[37m [39;49;00mcontent[37m [39;49;00mendpoints,[37m [39;49;00mexact-[34mmatch[39;49;00m[37m [39;49;00mCORS[37m [39;49;00mallowlist[37m [39;49;00mvia[37m [39;49;00m[04m[91m`[39;49;00mcors_origins[04m[91m`[39;49;00m[37m [39;49;00mconfig.[37m [39;49;00mHandlers[37m [39;49;00mdepend[37m [39;49;00mon[37m [39;49;00man[37m [39;49;00m[04m[91m`[39;49;00mapi.Store[04m[91m`[39;49;00m[37m [39;49;00minterface;[37m [39;49;00m[04m[91m`[39;49;00mServer.DB[04m[91m`[39;49;00m[37m [39;49;00mremains[37m [39;49;00monly[37m [39;49;00m[34mfor[39;49;00m[37m [39;49;00mthe[37m [39;49;00mreadiness[37m [39;49;00mprobe.[37m[39;49;00m
-[37m [39;49;00m**M5[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mthe[37m [39;49;00mAPI[37m [39;49;00mrunner[37m [39;49;00mretries[37m [39;49;00ma[37m [39;49;00mmid-stream[37m [39;49;00mfailure[37m [39;49;00mup[37m [39;49;00mto[37m [39;49;00m[34m3[39;49;00m[37m [39;49;00mtimes[37m [39;49;00mwith[37m [39;49;00mbackoff[37m [39;49;00m(cancellation[37m [39;49;00mexcepted);[37m [39;49;00mHTTP-level[37m [39;49;00mretries[37m [39;49;00mstay[37m [39;49;00mwith[37m [39;49;00mthe[37m [39;49;00mSDK.[37m[39;49;00m
-[37m [39;49;00m**M6[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00mmigration[37m [39;49;00mlock[37m [39;49;00macquisition[37m [39;49;00mbounded[37m [39;49;00mat[37m [39;49;00m[34m5[39;49;00m[37m [39;49;00mminutes[37m [39;49;00mwith[37m [39;49;00ma[37m [39;49;00mdiagnosis;[37m [39;49;00mon[37m [39;49;00munlock[37m [39;49;00mfailure[37m [39;49;00mthe[37m [39;49;00mconnection[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mdestroyed[37m [39;49;00mrather[37m [39;49;00mthan[37m [39;49;00mreturned[37m [39;49;00mto[37m [39;49;00mthe[37m [39;49;00mpool[37m [39;49;00mholding[37m [39;49;00mthe[37m [39;49;00mlock.[37m[39;49;00m
-[37m [39;49;00m**A3[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00mmemStore[04m[91m`[39;49;00m[37m [39;49;00mnow[37m [39;49;00mmirrors[37m [39;49;00mPostgres[37m [39;49;00msemantics:[37m [39;49;00mslug[37m [39;49;00m[34mis[39;49;00m[37m [39;49;00mthe[37m [39;49;00mpage[37m [39;49;00midentity[37m [39;49;00m(same-slug[37m [39;49;00mupsert[37m [39;49;00mreplaces[37m [39;49;00mthe[37m [39;49;00mrow[37m [39;49;00macross[37m [39;49;00mpaths),[37m [39;49;00mdeletion[37m [39;49;00mmatches[37m [39;49;00mslug[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mwell[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mpath,[37m [39;49;00martifacts[37m [39;49;00mstored[37m [39;49;00mwith[37m [39;49;00mreplace/append[37m [39;49;00msemantics.[37m[39;49;00m
-[37m [39;49;00m**A4[37m [39;49;00mfixed**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00m[04m[91m`[39;49;00m.github/workflows/ci.yml[04m[91m`[39;49;00m:[37m [39;49;00mhermetic[37m [39;49;00mjob[37m [39;49;00m(vet,[37m [39;49;00mgofmt,[37m [39;49;00m[04m[91m`[39;49;00mgo[37m [39;49;00mtest[37m [39;49;00m-race[04m[91m`[39;49;00m)[37m [39;49;00mplus[37m [39;49;00man[37m [39;49;00mintegration[37m [39;49;00mjob[37m [39;49;00magainst[37m [39;49;00ma[37m [39;49;00mPostgres[37m [39;49;00m[34m16[39;49;00m[37m [39;49;00mservice[37m [39;49;00mcontainer[37m [39;49;00mrunning[37m [39;49;00mthe[37m [39;49;00mfull[37m [39;49;00msuite.[37m [39;49;00m[04m[91m`[39;49;00mmake[37m [39;49;00mdev[04m[91m`[39;49;00m[37m [39;49;00mno[37m [39;49;00mlonger[37m [39;49;00mreferences[37m [39;49;00mthe[37m [39;49;00mremoved[37m [39;49;00mflag;[37m [39;49;00mREADME[37m [39;49;00mupdated[37m [39;49;00m(Go[37m [39;49;00m[34m1.25[39;49;00m,[37m [39;49;00mclaude[37m [39;49;00mCLI[37m [39;49;00moptional,[37m [39;49;00mhonest[37m [39;49;00marchitecture[37m [39;49;00msection).[37m[39;49;00m
[37m[39;49;00m
[37m### Left deliberately out of scope[39;49;00m[37m[39;49;00m
-[37m [39;49;00m**A2[37m [39;49;00m(job[37m [39;49;00mqueue[37m [39;49;00m/[37m [39;49;00mworker[37m [39;49;00mrole)**[37m [39;49;00m[04m[91m—[39;49;00m[37m [39;49;00ma[37m [39;49;00mnew[37m [39;49;00mfeature,[37m [39;49;00m[35mnot[39;49;00m[37m [39;49;00ma[37m [39;49;00mdefect[37m [39;49;00mfix.[37m [39;49;00mAll[37m [39;49;00mcode,[37m [39;49;00mflags,[37m [39;49;00mconfig,[37m [39;49;00m[35mand[39;49;00m[37m [39;49;00mdocs[37m [39;49;00mthat[37m [39;49;00mpretended[37m [39;49;00mit[37m [39;49;00mexisted[37m [39;49;00mhave[37m [39;49;00mbeen[37m [39;49;00mremoved[37m [39;49;00m[35mor[39;49;00m[37m [39;49;00mrelabeled[37m [39;49;00m[33m"[39;49;00m[33mplanned[39;49;00m[33m"[39;49;00m,[37m [39;49;00mper[37m [39;49;00mM1[33m'[39;49;00m[33ms reconcile-or-delete rule.[39;49;00m[37m[39;49;00m
-[37m [39;49;00m[04m[91m`[39;49;00mCascade.DeleteBlobs[04m[91m`[39;49;00m[37m [39;49;00mremains[37m [39;49;00mcomputed-but-unconsumed,[37m [39;49;00mnow[37m [39;49;00mexplicitly[37m [39;49;00mdocumented[37m [39;49;00m[34mas[39;49;00m[37m [39;49;00mawaiting[37m [39;49;00mthe[37m [39;49;00mobject-storage[37m [39;49;00msubsystem.[37m[39;49;00m
