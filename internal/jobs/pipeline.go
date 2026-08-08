package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// Budget bounds one run's spend.
type Budget struct {
	AnalyzeUSD float64
	PageUSD    float64
	RunUSD     float64
	MaxPages   int
}

// Pipeline executes builds.
type Pipeline struct {
	Store  Store
	Runner agent.Runner
	Budget Budget
	Log    *slog.Logger

	Model         string
	AnalyzeModel  string
	FallbackModel string
	Timeout       time.Duration

	// Blobs, when set, deletes stored blobs the deletion cascade released.
	// Nil (no object storage) leaves DeleteBlobs as computed-but-inert data,
	// the pre-blob-store behavior. A narrow local interface rather than
	// blob.Store: the pipeline only ever deletes.
	Blobs interface {
		Delete(ctx context.Context, key string) error
	}

	// MaxRetries is the number of corrective attempts after a validation
	// failure. The first re-states the errors; the second halves the work.
	MaxRetries int
	// UnitConcurrency is how many units may generate at once. Values below 2
	// keep the pipeline sequential, which is the default: fan-out multiplies
	// in-flight model calls, so the run budget is what bounds spend rather
	// than the one-call-at-a-time shape of the loop.
	UnitConcurrency int
	// WarnTurns logs a warning when a single agent call uses more turns than
	// this. Zero disables the check.
	WarnTurns int
	// Now is injectable so tests can pin dates in frontmatter.
	Now func() time.Time
}

// BuildRequest is one pipeline invocation.
type BuildRequest struct {
	RunID       string
	WorkspaceID string
	Trigger     string
	Ref         string
	// Connectors attributes each source record to the connector whose sync
	// produced it, by namespace. Zero value for CLI builds, which have no
	// connector rows.
	Connectors SourceConnectors

	// SyncedNamespaces lists the source namespaces this run actually synced
	// (see diff.Namespace). Deletion candidates are only raised inside them:
	// a build without --docs must not read uploaded documents as deleted,
	// while a synced-but-empty namespace is exactly a deletion. Nil infers
	// from surviving units, the pre-M6 behavior.
	SyncedNamespaces []string

	// SourceDir is the materialized tree the agent reads. It is never written
	// to; validation re-checks it afterwards to prove that held.
	SourceDir string
	// ScratchDir is the only writable location granted to the agent.
	ScratchDir string

	// Map is the freshly built partitioning of the sources.
	Map *mapper.WorkspaceMap
	// Router attributes changed paths to units.
	Router diff.Router
	// Changes is the difference from the last successful run.
	Changes diff.ChangeSet

	// ApprovedDeletions are source keys a human confirmed for removal. Nothing
	// is ever deleted without this, because a suspended token and a genuine
	// deletion look identical at the sync layer.
	ApprovedDeletions []diff.Key

	// BlobKeys names the stored blobs behind each uploaded document's unit
	// key, so the source records carry them and an approved deletion can
	// cascade to storage. Section units inherit their parent document's blobs.
	// Nil for sources that live outside the blob store (CLI --docs, repos).
	BlobKeys map[string][]string

	// SkippedKeys are unit keys deliberately left out of this sync (paused
	// documents): absent from the map by choice, so deletion detection must
	// pass over them and their section children.
	SkippedKeys []diff.Key

	// WorkspaceSlug names the bench for anything a human reads. WorkspaceID is
	// a UUID, and putting it on the overview -- the first page anyone opens --
	// labelled the wiki with a string that identifies nothing to its reader.
	// Empty falls back to the id, so a hand-built request still renders.
	WorkspaceSlug string

	// Force skips the content-hash gate so every routed unit regenerates, for
	// recovering from bad output or a prompt change.
	Force bool

	// DryRun plans and estimates without invoking the agent at all.
	DryRun bool
}

// BuildResult reports what a run did.
type BuildResult struct {
	Summary RunSummary
	// Planned is what would have run, populated for dry runs and for the
	// estimate-and-confirm preview.
	Planned []diff.Key
	// EstimatedUSD is the projected spend shown before anything is charged.
	EstimatedUSD float64
	// Deferred counts units the per-run page cap held back. They stay stale and
	// are picked up by the next run.
	Deferred   int
	Violations []wiki.Violation
}

// defaultEstimatePerUnit is used until a workspace has real cost history.
const defaultEstimatePerUnit = 0.12

// workspaceLabel is the bench's name for human-facing output, falling back to
// the id when no slug was supplied.
func (r BuildRequest) workspaceLabel() string {
	if r.WorkspaceSlug != "" {
		return r.WorkspaceSlug
	}
	return r.WorkspaceID
}

// Build runs the pipeline.
func (p *Pipeline) Build(ctx context.Context, req BuildRequest) (*BuildResult, error) {
	log := p.logger().With("run", req.RunID, "workspace", req.WorkspaceID)
	now := p.now()

	res := &BuildResult{
		Summary: RunSummary{
			RunID: req.RunID, WorkspaceID: req.WorkspaceID,
			Trigger: req.Trigger, Ref: req.Ref, Status: StatusRunning,
		},
	}

	if err := p.checkBudgetEnforceable(); err != nil {
		return nil, err
	}

	sources, err := p.Store.LoadSources(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load sources: %w", err)
	}
	pages, err := p.Store.LoadPages(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load pages: %w", err)
	}

	// Deletions approved through the review queue join whatever the caller
	// passed explicitly. The queue is how a human answers the question a
	// disappeared source raises; the explicit list remains for automation.
	approved, err := p.Store.LoadApprovedDeletions(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load approved deletions: %w", err)
	}
	req.ApprovedDeletions = mergeKeys(req.ApprovedDeletions, approved)

	// A source on record but absent from the map has disappeared. That is a
	// question, not a command -- file a review item and leave everything in
	// place until a human answers it. Filed before the no-changes early return,
	// because a vanished source is often the *only* thing that changed.
	if !req.DryRun {
		var synced map[string]bool
		if req.SyncedNamespaces != nil {
			synced = make(map[string]bool, len(req.SyncedNamespaces))
			for _, ns := range req.SyncedNamespaces {
				synced[ns] = true
			}
		}
		cands := deletionCandidates(sources, req.Map, req.ApprovedDeletions, req.SkippedKeys, synced)
		if len(cands) > 0 {
			if err := p.Store.EnsureDeletionReviews(ctx, req.WorkspaceID, cands); err != nil {
				return nil, fmt.Errorf("jobs: file deletion reviews: %w", err)
			}
			log.Info("filed deletion reviews for disappeared sources", "count", len(cands))
		}
	}

	// Route changes to dirty units, then drop any whose content hash is
	// unchanged. This gate is the primary cost control: an unchanged workspace
	// costs nothing, so sweeps and no-op webhooks are free.
	plan := req.Router.Route(req.Changes)
	dirty := plan.Dirty
	if req.Force {
		log.Info("hash gate skipped; every routed unit regenerates", "units", len(dirty))
	} else {
		dirty = p.filterUnchanged(dirty, req.Map, sources)
	}

	cascade := diff.PlanCascade(sources, req.ApprovedDeletions)
	// Pages that survive a deletion but still describe the departed source are
	// regenerated, not merely kept: leaving them is how a wiki accumulates
	// confident claims about deleted code.
	dirty = append(dirty, regenKeysFor(cascade, sources, dirty)...)

	if len(dirty) == 0 && cascade.Empty() {
		log.Info("nothing to do; no agent calls")
		res.Summary.Status = StatusNoChanges
		return res, p.Store.RecordRun(ctx, res.Summary)
	}

	if p.Budget.MaxPages > 0 && len(dirty) > p.Budget.MaxPages {
		log.Warn("truncating work to the per-run page cap",
			"planned", len(dirty), "cap", p.Budget.MaxPages)
		// Recorded rather than only logged: the preview is the number someone
		// approves a spend against, and a plan that quietly covers half the
		// repository would be confirmed as though it covered all of it.
		res.Deferred = len(dirty) - p.Budget.MaxPages
		dirty = truncatePreservingArch(dirty, p.Budget.MaxPages)
	}

	// The estimate prefers this workspace's own cost history over the global
	// constant: previews are what humans approve spend against, and a
	// workspace whose pages run expensive should say so before, not after.
	perUnit, err := p.Store.TrailingUnitCost(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: trailing unit cost: %w", err)
	}
	if perUnit <= 0 {
		perUnit = defaultEstimatePerUnit
	}

	res.Planned = dirty
	res.EstimatedUSD = float64(len(dirty)) * perUnit

	// Nothing is charged before a human sees the estimate.
	if req.DryRun {
		res.Summary.Status = StatusPending
		return res, nil
	}

	// The plan becomes visible before it becomes expensive. Until this existed
	// a build in flight reported only "running", so a bench ingesting a large
	// repository looked identical five seconds and five minutes in, with no way
	// to tell how much was left.
	//
	// Best-effort throughout: a build that generated pages correctly must not
	// fail because its progress could not be written down.
	if err := p.Store.SeedRunItems(ctx, req.RunID, dirty, perUnit); err != nil {
		log.Warn("could not record the plan; the build runs but cannot be watched", "err", err)
	}

	steering, err := p.Store.LoadSteering(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load steering: %w", err)
	}

	known := knownSlugs(pages)
	units := unitsByKey(req.Map)

	// Slug is the database's page identity and Created survives regeneration.
	// Both are snapshots taken before generation: units may run concurrently,
	// so what the sequential pipeline accumulated here as it went is settled
	// afterwards, in mergeOutcomes.
	existingBySlug := make(map[string]string, len(pages))
	existingCreated := make(map[string]string, len(pages))
	for _, pg := range pages {
		existingBySlug[pg.Slug] = pg.Path
		existingCreated[pg.Slug] = pg.Meta.Created
	}

	conc := p.unitConcurrency()
	ledger := newBudgetLedger(p.Budget.RunUSD)
	outcomes, halted := p.generateUnits(ctx, req, dirty, units, conc, unitScope{
		steering:        steering,
		known:           known,
		existingBySlug:  existingBySlug,
		existingCreated: existingCreated,
		ledger:          ledger,
		estPerCall:      perUnit,
		deferLinks:      conc > 1,
	}, log)
	if halted != "" {
		res.Summary.Status = halted
	}

	written, newSources, findings, spent := p.mergeOutcomes(res, req, outcomes, known, perUnit, log)
	res.Summary.CostUSD = spent

	// Post-pass: rebuild the derived artifacts from what pages now exist. The
	// agent never writes these, so they cannot drift from the content.
	merged := mergePages(pages, written, cascade.DeletePages)
	index := wiki.BuildIndex(merged, wiki.IndexOptions{})
	overview := wiki.BuildOverview(merged, wiki.OverviewInput{
		Workspace: req.workspaceLabel(),
		Ref:       req.Ref,
		Date:      now.Format(wiki.DateFormat),
		Narrative: findings,
		Sources:   sourceTallies(req.Map),
	})

	created, updated := countChanges(pages, written)
	res.Summary.Created = created
	res.Summary.Updated = updated
	res.Summary.Deleted = len(cascade.DeletePages)

	logEntry := wiki.RenderLogEntry(wiki.LogEntry{
		Date:    now.Format(wiki.DateFormat),
		Action:  "build",
		Subject: req.WorkspaceID,
		Ref:     req.Ref,
		Lines:   buildLogLines(res.Summary, res.Summary.CostUSD),
	})

	// Import and the run record survive cancellation: the money is already
	// spent, so a canceled run must still persist what it produced and what it
	// cost.
	importCtx := context.WithoutCancel(ctx)

	if err := p.Store.Import(importCtx, ImportRequest{
		WorkspaceID:     req.WorkspaceID,
		RunID:           req.RunID,
		UpsertPages:     written,
		SoftDeletePages: cascade.DeletePages,
		UpsertSources:   newSources,
		DropSources:     cascade.DropSources,
		Index:           index,
		Overview:        overview,
		LogEntry:        logEntry,
	}); err != nil {
		// The agent calls above are already billed; a run that fails at import
		// must still be ledgered or budget windows undercount forever.
		res.Summary.Status = StatusFailed
		res.Summary.Err = "import: " + err.Error()
		if rerr := p.Store.RecordRun(importCtx, res.Summary); rerr != nil {
			log.Error("run could not be ledgered after import failure", "err", rerr)
		}
		return nil, fmt.Errorf("jobs: import: %w", err)
	}

	// Blob GC rides the same post-import moment as the page cascade: only
	// after the sources that referenced these blobs are durably dropped is
	// deleting them safe. Best effort -- the blob store's deletes are
	// idempotent, and a failure leaves an orphan for a later sweep, never a
	// dangling reference.
	if p.Blobs != nil {
		for _, key := range cascade.DeleteBlobs {
			if err := p.Blobs.Delete(importCtx, key); err != nil {
				log.Error("blob delete failed; orphan left for GC", "key", key, "err", err)
			}
		}
	}

	if res.Summary.Status == StatusRunning {
		res.Summary.Status = runStatus(res.Summary.Items)
	}
	if res.Summary.Err == "" && res.Summary.Status != StatusSucceeded && res.Summary.Status != StatusNoChanges {
		for _, it := range res.Summary.Items {
			if it.Err != "" {
				res.Summary.Err = fmt.Sprintf("%s: %s", it.Key, it.Err)
				break
			}
		}
	}
	return res, p.Store.RecordRun(importCtx, res.Summary)
}

// checkBudgetEnforceable refuses to start a budgeted run whose costs would
// estimate to zero. On runners that estimate from the pricing table, an
// unknown model id prices every call at $0 and silently disables the budget --
// the opposite of what configuring a budget asked for. CLI-reported costs are
// authoritative, so the check does not apply there.
func (p *Pipeline) checkBudgetEnforceable() error {
	if p.Budget.RunUSD <= 0 || !agent.EstimatesCost(p.Runner) {
		return nil
	}
	for _, m := range []string{p.Model, p.AnalyzeModel, p.FallbackModel} {
		if m != "" && !agent.KnownModel(m) {
			return fmt.Errorf(
				"jobs: run budget cannot be enforced: model %q has no pricing entry (fix the model id or add pricing)", m)
		}
	}
	return nil
}

// filterUnchanged drops units whose content hash matches what the last
// successful run recorded. This is where a no-op run becomes free.
func (p *Pipeline) filterUnchanged(dirty []diff.Key, m *mapper.WorkspaceMap, sources []diff.SourceRecord) []diff.Key {
	prior := make(map[diff.Key]string, len(sources))
	for _, s := range sources {
		prior[s.Key] = s.InputHash
	}
	units := unitsByKey(m)

	out := make([]diff.Key, 0, len(dirty))
	for _, k := range dirty {
		var hash string
		if k == diff.ArchOverview {
			// The synthesis has no unit of its own; its input is the whole
			// map, so the map hash gates it. Without this every build --
			// including a completely unchanged one -- would pay for an
			// architecture regeneration.
			if m != nil {
				hash = m.Hash
			}
		} else if unit, ok := units[string(k)]; ok {
			hash = unit.Hash
		}
		// No hash means no basis for skipping: regenerate.
		if hash == "" || prior[k] != hash {
			out = append(out, k)
		}
	}
	return out
}

// unitResult is one unit's outcome. A struct rather than a long return list:
// the caller needs pages, findings, cost, turns, and two distinct failure modes,
// and positional returns stop being readable well before that.
type unitResult struct {
	Pages    []wiki.Page
	Findings []string
	// Reviews are flags the agent raised for human judgment, from both the
	// analyze and generate steps.
	Reviews    []agent.ReviewFlag
	CostUSD    float64
	Turns      int
	Tokens     int
	Violations []wiki.Violation
	Err        error
}

// errRunBudgetExhausted marks a unit that could not reserve budget for an
// agent call. Sentinel rather than a formatted error because the scheduler
// distinguishes it from a genuine failure: it stops launching further units
// instead of recording a fault against this one.
var errRunBudgetExhausted = errors.New("run budget exhausted")

// unitScope is the run-level context a unit is generated against. Every map
// in it is a snapshot taken before generation begins and is read-only for
// the duration: units may run concurrently, so nothing here may be mutated
// as they land. What the sequential pipeline accumulated in these maps is
// now settled after every unit finishes, in mergeOutcomes.
type unitScope struct {
	steering Steering
	// known resolves wikilinks against pages that existed when the run
	// started. Pages written by *this* run are not here -- a concurrent unit
	// cannot see them -- so the link check is deferred to mergeOutcomes,
	// where the run's full slug set is finally known.
	known map[string]bool
	// existingBySlug rejects slug collisions against live pages, since slug is
	// the database's page identity. Collisions between two units of the same
	// run are caught in mergeOutcomes instead.
	existingBySlug map[string]string
	// existingCreated preserves a page's original Created date across
	// regeneration.
	existingCreated map[string]string
	// ledger enforces the run budget across concurrent units by reserving
	// before each agent call rather than accumulating after it.
	ledger *budgetLedger
	// estPerCall is the fallback reservation when no per-call budget is
	// configured, so a run budget still bounds something.
	estPerCall float64
	// deferLinks moves the unresolved-wikilink check out of the retry loop
	// and into mergeOutcomes. Set only when units actually run concurrently:
	// at concurrency 1 the sequential pipeline's growing slug set is both
	// available and strictly better, because a dangling link caught inside
	// the loop is stated back to the agent and fixed within the run rather
	// than failing the unit until the next one.
	deferLinks bool
}

// costOf reads a call's cost defensively: a runner that fails may return a
// nil result, and the reservation must still be settled with something.
func costOf(res *agent.Result) float64 {
	if res == nil {
		return 0
	}
	return res.TotalCostUSD
}

// account folds one agent call's telemetry into the unit, and is called
// before the caller decides whether that call failed.
//
// A failing runner can still return a populated result: the CLI runner does
// exactly that when the process exits non-zero despite a success envelope,
// and the envelope carries the cost of work that was really performed and
// really billed. Counting only on the success path dropped that money from
// the run summary while the ledger settled it correctly, so the run budget
// and the recorded spend disagreed about the same call -- and the workspace
// budget window, which reads what was recorded, undercounted it forever.
//
// Nil-safe, because the other failure mode is no result at all.
func (p *Pipeline) account(res *unitResult, key diff.Key, call *agent.Result) {
	if call == nil {
		return
	}
	res.CostUSD += call.TotalCostUSD
	res.Turns += call.NumTurns
	res.Tokens += call.Usage.Total()
	p.noteAgentEvents(key, call)
}

// generateUnit runs analyze once and then generate for one unit, retrying
// generation on validation failure with the errors stated back to the agent.
// The analysis is not re-run on retry: only generation failed, and the plan
// does not change because a page body was malformed.
func (p *Pipeline) generateUnit(
	ctx context.Context,
	req BuildRequest,
	key diff.Key,
	unit mapper.Unit,
	sc unitScope,
) unitResult {
	var res unitResult

	// Only a runner that writes files needs somewhere to write them. The API
	// runner returns pages as data, so no scratch directory is created at all.
	scratch := ""
	if req.ScratchDir != "" {
		scratch = filepath.Join(req.ScratchDir, sanitize(string(key)))
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			res.Err = fmt.Errorf("create scratch dir: %w", err)
			return res
		}
	}

	sessionID := req.RunID + "-" + sanitize(string(key))

	// The unit's own root, not the run's. A merged map has no single one: repo
	// units are relative to the checkout, while uploaded documents and fetched
	// web pages are staged elsewhere and record where. Prompt assembly has
	// always honoured that (see unitRoot); the agent's working directory did
	// not, which broke two ways on the CLI runner. A bench of only documents
	// has no checkout at all, so WorkDir was empty and every unit failed with
	// "agent: WorkDir is required"; a bench with both pointed the agent's
	// Read and Grep at the repository while asking it about a PDF.
	workDir := unitRoot(req.SourceDir, unit)

	// Reserved before the call, settled after. A unit that cannot reserve its
	// analyze call has not spent anything, so it is not a failure -- the
	// scheduler reads the sentinel and simply stops launching work, leaving
	// this unit stale for the next run.
	want, ok := sc.ledger.reserveOr(p.Budget.AnalyzeUSD, sc.estPerCall)
	if !ok {
		res.Err = errRunBudgetExhausted
		return res
	}
	analyzeRes, aerr := p.Runner.Run(ctx, agent.Request{
		Step: agent.StepAnalyze, SessionID: sessionID,
		WorkDir: workDir, Model: p.AnalyzeModel,
		BudgetUSD: p.Budget.AnalyzeUSD, Timeout: p.Timeout,
		SystemPrompt: analyzeSystemPrompt(sc.steering),
		// The rendered map is identical for every unit in the run, so it
		// rides the cache breakpoint and all but the first unit reads it at
		// a fraction of the input rate.
		CacheableContext: req.Map.Summary,
		Prompt:           analyzePrompt(key, unit, req.SourceDir, sc.steering, 0, nil),
		JSONSchema:       AnalysisSchema,
	})
	sc.ledger.settle(want, costOf(analyzeRes))
	p.account(&res, key, analyzeRes)
	if aerr != nil {
		res.Err = aerr
		return res
	}
	if err := analyzeRes.Err(); err != nil {
		res.Err = err
		return res
	}
	// The analysis is paid for and the unit has a way to go. Publishing here is
	// what makes a single-unit ingest observable at all: the settle in fanout
	// reports the unit's whole spend at once, so a bench with one document
	// showed nothing for the entire run and then everything, which reads as
	// "this is not costing anything" right up until it reads as "that is what
	// it cost".
	p.publishSpend(ctx, req.RunID, key, &res)

	// The plan is handed to generation explicitly. The CLI runner also resumes
	// the session, but the default API runner is stateless -- without this the
	// analyze step would be paid for and never read.
	plan, planned, analyzeReviews := planFrom(analyzeRes)
	res.Reviews = append(res.Reviews, analyzeReviews...)

	attempts := p.MaxRetries + 1
	var lastViolations []wiki.Violation

	for attempt := range attempts {
		// Past the analyze call this unit has already spent money, so running
		// out here is a failure worth recording against it -- unlike the
		// reservation above, which means it never started.
		want, ok := sc.ledger.reserveOr(p.Budget.PageUSD, sc.estPerCall)
		if !ok {
			res.Err = fmt.Errorf("%w mid-unit after $%.4f", errRunBudgetExhausted, res.CostUSD)
			return res
		}

		genRes, gerr := p.Runner.Run(ctx, agent.Request{
			Step: agent.StepGenerate, SessionID: sessionID,
			WorkDir: workDir, ScratchDir: scratch,
			Model: p.modelFor(attempt), FallbackModel: p.FallbackModel,
			BudgetUSD: p.Budget.PageUSD, Timeout: p.Timeout,
			SystemPrompt:     generateSystemPrompt(sc.steering),
			CacheableContext: req.Map.Summary,
			Prompt:           generatePrompt(key, unit, req.SourceDir, scratch, sc.steering, plan, attempt, lastViolations),
		})
		sc.ledger.settle(want, costOf(genRes))
		p.account(&res, key, genRes)
		if gerr != nil {
			res.Err = gerr
			return res
		}
		if err := genRes.Err(); err != nil {
			res.Err = err
			return res
		}

		// Runners deliver pages two ways: the API runner returns them as
		// structured data, the CLI runner writes them into the scratch
		// directory. Everything after this point is identical, which is what
		// lets both satisfy one interface.
		collected, parseViolations, cerr := pagesFrom(genRes, scratch)
		if cerr != nil {
			res.Err = cerr
			return res
		}

		p.stampDates(collected, sc.existingCreated)

		lastViolations = append(parseViolations, wiki.ValidateBatch(collected, wiki.ValidateOptions{
			KnownSlugs:     sc.known,
			ExistingBySlug: sc.existingBySlug,
			Planned:        planned,
			RequireDates:   true,
			DeferLinkCheck: sc.deferLinks,
		})...)
		if len(lastViolations) == 0 {
			res.Pages = derefPages(collected)
			if genRes.Generation != nil {
				res.Findings = genRes.Generation.Findings
				res.Reviews = append(res.Reviews, genRes.Generation.Reviews...)
			}
			return res
		}

		p.logger().Warn("validation failed; retrying",
			"key", key, "attempt", attempt+1, "violations", len(lastViolations))
		// A retry is the other moment a unit has spent more and is not done.
		// Without this the extra attempts a struggling unit pays for are
		// invisible until it either succeeds or gives up.
		p.publishSpend(ctx, req.RunID, key, &res)
		// Clear the scratch dir so a partial attempt is not re-collected.
		if scratch != "" {
			if err := os.RemoveAll(scratch); err != nil {
				res.Violations, res.Err = lastViolations, err
				return res
			}
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				res.Violations, res.Err = lastViolations, err
				return res
			}
		}
	}

	res.Violations = lastViolations
	return res
}

// planFrom extracts the analysis for the generate prompt: the serialized plan,
// the set of paths it authorized -- which validation enforces as the
// quarantine boundary on what the agent may write -- and any review flags the
// analyze step raised.
func planFrom(res *agent.Result) (plan string, planned map[string]bool, reviews []agent.ReviewFlag) {
	analysis := res.Analysis
	if analysis == nil {
		// The CLI runner returns the schema-constrained JSON as envelope text.
		parsed, err := agent.ParseAnalysis(res.Result)
		if err != nil {
			// Unparseable analysis text still carries signal; pass it through
			// verbatim with no quarantine rather than dropping it.
			return strings.TrimSpace(res.Result), nil, nil
		}
		analysis = parsed
	}

	if b, err := json.Marshal(analysis); err == nil {
		plan = string(b)
	}
	if len(analysis.Pages) > 0 {
		planned = make(map[string]bool, len(analysis.Pages))
		for _, pg := range analysis.Pages {
			planned[pg.Path] = true
		}
	}
	return plan, planned, analysis.Reviews
}

// sourceTallies counts the map's units by the namespace that produced them, so
// the overview can say what the bench is made of. Derived from the map on every
// build rather than stored, which is what keeps the sentence true as sources
// are connected and removed.
//
// Order is the namespaces' own, not a map iteration: the overview is rewritten
// on every run and compared against the last, so an unstable order would read
// as a change on a build where nothing changed.
func sourceTallies(m *mapper.WorkspaceMap) []wiki.SourceTally {
	if m == nil {
		return nil
	}
	counts := map[string]int{}
	for _, u := range m.Units {
		counts[diff.Namespace(diff.Key(u.Key))]++
	}
	order := []string{"module", "doc", "doc:upload", "doc:web"}
	out := make([]wiki.SourceTally, 0, len(order))
	for _, kind := range order {
		if n := counts[kind]; n > 0 {
			out = append(out, wiki.SourceTally{Kind: kind, Count: n})
		}
	}
	return out
}

// stampDates fills created/updated. Updated is always the run date -- the page
// was regenerated today -- while Created survives from the page being replaced,
// which is the contract the frontmatter documents.
func (p *Pipeline) stampDates(pages []*wiki.Page, existingCreated map[string]string) {
	today := p.now().Format(wiki.DateFormat)
	for _, pg := range pages {
		if pg.Meta.Created == "" {
			if c := existingCreated[pg.Slug]; c != "" {
				pg.Meta.Created = c
			} else {
				pg.Meta.Created = today
			}
		}
		pg.Meta.Updated = today
	}
}

// noteAgentEvents surfaces per-call telemetry that would otherwise be
// swallowed: sandbox denials usually mean the agent tried something the design
// forbids, and an unusual turn count is the early sign of a runaway session.
func (p *Pipeline) noteAgentEvents(key diff.Key, res *agent.Result) {
	if n := len(res.PermissionDenials); n > 0 {
		tools := make([]string, 0, n)
		for _, d := range res.PermissionDenials {
			tools = append(tools, d.ToolName)
		}
		p.logger().Warn("sandbox denied agent tool calls", "key", key, "count", n, "tools", tools)
	}
	if p.WarnTurns > 0 && res.NumTurns > p.WarnTurns {
		p.logger().Warn("agent call used an unusual number of turns",
			"key", key, "turns", res.NumTurns, "warn_at", p.WarnTurns)
	}
	if res.OverBudget {
		p.logger().Warn("agent call exceeded its per-call budget", "key", key, "cost", res.TotalCostUSD)
	}
}

// modelFor escalates to the fallback model on the final attempt rather than
// starting expensive.
func (p *Pipeline) modelFor(attempt int) string {
	if attempt > 0 && p.FallbackModel != "" && attempt >= p.MaxRetries {
		return p.FallbackModel
	}
	return p.Model
}

func (p *Pipeline) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}
