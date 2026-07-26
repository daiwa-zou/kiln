package jobs

import (
	"context"
	"encoding/json"
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

	// MaxRetries is the number of corrective attempts after a validation
	// failure. The first re-states the errors; the second halves the work.
	MaxRetries int
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
		cands := deletionCandidates(sources, req.Map, req.ApprovedDeletions)
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

	res.Planned = dirty
	res.EstimatedUSD = float64(len(dirty)) * defaultEstimatePerUnit

	// Nothing is charged before a human sees the estimate.
	if req.DryRun {
		res.Summary.Status = StatusPending
		return res, nil
	}

	steering, err := p.Store.LoadSteering(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load steering: %w", err)
	}

	known := knownSlugs(pages)
	units := unitsByKey(req.Map)

	// Slug is the database's page identity and Created survives regeneration;
	// both maps are maintained through the loop so a later unit sees pages an
	// earlier unit just wrote.
	existingBySlug := make(map[string]string, len(pages))
	existingCreated := make(map[string]string, len(pages))
	for _, pg := range pages {
		existingBySlug[pg.Slug] = pg.Path
		existingCreated[pg.Slug] = pg.Meta.Created
	}

	var (
		written    []wiki.Page
		newSources []diff.SourceRecord
		// findings are the agent's narrative observations. They are the one
		// part of the overview the model contributes; the structure around
		// them stays derived.
		findings []string
		spent    float64
	)

	for _, key := range dirty {
		if ctx.Err() != nil {
			// Break rather than fail: units already generated are still valid
			// and are imported below on an uncancelable context, so a Ctrl-C
			// costs the remainder of the run, not the work already paid for.
			log.Warn("run canceled; importing what completed", "cause", ctx.Err())
			res.Summary.Status = StatusCanceled
			break
		}
		if p.Budget.RunUSD > 0 && spent >= p.Budget.RunUSD {
			// Stop scheduling rather than failing: work already imported is
			// good, and the remainder keeps its stale hash so it retries.
			log.Warn("run budget exhausted; remaining units stay stale",
				"spent", spent, "budget", p.Budget.RunUSD)
			res.Summary.Status = StatusOverBudget
			break
		}

		var remaining float64
		if p.Budget.RunUSD > 0 {
			remaining = p.Budget.RunUSD - spent
		}

		unit := units[string(key)]
		item := ItemSummary{Key: key, Status: StatusPending}

		out := p.generateUnit(ctx, req, key, unit, unitScope{
			steering:        steering,
			known:           known,
			existingBySlug:  existingBySlug,
			existingCreated: existingCreated,
			remainingUSD:    remaining,
		})
		spent += out.CostUSD
		item.CostUSD = out.CostUSD
		item.Turns = out.Turns
		res.Summary.Tokens += out.Tokens

		switch {
		case out.Err != nil:
			item.Status = StatusFailed
			item.Err = out.Err.Error()
			log.Error("unit failed", "key", key, "err", out.Err)
		case len(out.Violations) > 0:
			item.Status = StatusFailed
			item.Err = fmt.Sprintf("%d validation violations", len(out.Violations))
			res.Violations = append(res.Violations, out.Violations...)
			log.Error("unit failed validation", "key", key, "violations", len(out.Violations))
		default:
			item.Status = StatusSucceeded
			written = append(written, out.Pages...)
			findings = append(findings, out.Findings...)

			// Review flags ride the run summary and are persisted with it.
			// Only successful units contribute: a failed unit retries next run
			// and will raise its flags again alongside content that landed.
			for _, rf := range out.Reviews {
				res.Summary.Reviews = append(res.Summary.Reviews, ReviewNote{
					Kind: rf.Kind, Title: rf.Title, Detail: rf.Detail, Unit: key,
				})
			}

			// The source record is only updated on success, so a failed unit
			// keeps its old hash and is retried on the next run. The
			// architecture synthesis has no unit of its own; the whole-map
			// hash is its input, and recording it is what makes an unchanged
			// repeat build genuinely free.
			hash := unit.Hash
			if key == diff.ArchOverview {
				hash = req.Map.Hash
			}
			newSources = append(newSources, diff.SourceRecord{
				Key:          key,
				InputHash:    hash,
				FilesWritten: pagePaths(out.Pages),
			})
			for _, pg := range out.Pages {
				known[pg.Slug] = true
				existingBySlug[pg.Slug] = pg.Path
				existingCreated[pg.Slug] = pg.Meta.Created
			}
		}

		res.Summary.Items = append(res.Summary.Items, item)
	}

	res.Summary.CostUSD = spent

	// Post-pass: rebuild the derived artifacts from what pages now exist. The
	// agent never writes these, so they cannot drift from the content.
	merged := mergePages(pages, written, cascade.DeletePages)
	index := wiki.BuildIndex(merged, wiki.IndexOptions{})
	overview := wiki.BuildOverview(merged, wiki.OverviewInput{
		Workspace: req.WorkspaceID,
		Ref:       req.Ref,
		Date:      now.Format(wiki.DateFormat),
		Narrative: findings,
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
		Lines:   buildLogLines(res.Summary, spent),
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

// unitScope is the run-level context a unit is generated against.
type unitScope struct {
	steering Steering
	// known resolves wikilinks: existing slugs plus everything written so far
	// in this run.
	known map[string]bool
	// existingBySlug rejects slug collisions against live pages, since slug is
	// the database's page identity.
	existingBySlug map[string]string
	// existingCreated preserves a page's original Created date across
	// regeneration.
	existingCreated map[string]string
	// remainingUSD is what is left of the run budget; zero means unlimited.
	remainingUSD float64
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
	overBudget := func() bool { return sc.remainingUSD > 0 && res.CostUSD >= sc.remainingUSD }

	analyzeRes, aerr := p.Runner.Run(ctx, agent.Request{
		Step: agent.StepAnalyze, SessionID: sessionID,
		WorkDir: req.SourceDir, Model: p.AnalyzeModel,
		BudgetUSD: p.Budget.AnalyzeUSD, Timeout: p.Timeout,
		SystemPrompt: analyzeSystemPrompt(sc.steering),
		// The rendered map is identical for every unit in the run, so it
		// rides the cache breakpoint and all but the first unit reads it at
		// a fraction of the input rate.
		CacheableContext: req.Map.Summary,
		Prompt:           analyzePrompt(key, unit, req.SourceDir, sc.steering, 0, nil),
		JSONSchema:       AnalysisSchema,
	})
	if aerr != nil {
		res.Err = aerr
		return res
	}
	res.CostUSD += analyzeRes.TotalCostUSD
	res.Turns += analyzeRes.NumTurns
	res.Tokens += analyzeRes.Usage.Total()
	p.noteAgentEvents(key, analyzeRes)
	if err := analyzeRes.Err(); err != nil {
		res.Err = err
		return res
	}

	// The plan is handed to generation explicitly. The CLI runner also resumes
	// the session, but the default API runner is stateless -- without this the
	// analyze step would be paid for and never read.
	plan, planned, analyzeReviews := planFrom(analyzeRes)
	res.Reviews = append(res.Reviews, analyzeReviews...)

	attempts := p.MaxRetries + 1
	var lastViolations []wiki.Violation

	for attempt := range attempts {
		if overBudget() {
			res.Err = fmt.Errorf("run budget exhausted mid-unit after $%.4f", res.CostUSD)
			return res
		}

		genRes, gerr := p.Runner.Run(ctx, agent.Request{
			Step: agent.StepGenerate, SessionID: sessionID,
			WorkDir: req.SourceDir, ScratchDir: scratch,
			Model: p.modelFor(attempt), FallbackModel: p.FallbackModel,
			BudgetUSD: p.Budget.PageUSD, Timeout: p.Timeout,
			SystemPrompt:     generateSystemPrompt(sc.steering),
			CacheableContext: req.Map.Summary,
			Prompt:           generatePrompt(key, unit, req.SourceDir, sc.steering, plan, attempt, lastViolations),
		})
		if gerr != nil {
			res.Err = gerr
			return res
		}
		res.CostUSD += genRes.TotalCostUSD
		res.Turns += genRes.NumTurns
		res.Tokens += genRes.Usage.Total()
		p.noteAgentEvents(key, genRes)
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
