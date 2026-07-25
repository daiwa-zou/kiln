package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	Violations   []wiki.Violation
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

	sources, err := p.Store.LoadSources(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load sources: %w", err)
	}
	pages, err := p.Store.LoadPages(ctx, req.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: load pages: %w", err)
	}

	// Route changes to dirty units, then drop any whose content hash is
	// unchanged. This gate is the primary cost control: an unchanged workspace
	// costs nothing, so sweeps and no-op webhooks are free.
	plan := req.Router.Route(req.Changes)
	dirty := p.filterUnchanged(plan.Dirty, req.Map, sources)

	cascade := diff.PlanCascade(sources, req.ApprovedDeletions)

	if len(dirty) == 0 && cascade.Empty() {
		log.Info("nothing to do; no agent calls")
		res.Summary.Status = StatusNoChanges
		return res, p.Store.RecordRun(ctx, res.Summary)
	}

	if p.Budget.MaxPages > 0 && len(dirty) > p.Budget.MaxPages {
		log.Warn("truncating work to the per-run page cap",
			"planned", len(dirty), "cap", p.Budget.MaxPages)
		dirty = dirty[:p.Budget.MaxPages]
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

	var (
		written    []wiki.Page
		newSources []diff.SourceRecord
		spent      float64
	)

	for _, key := range dirty {
		if p.Budget.RunUSD > 0 && spent >= p.Budget.RunUSD {
			// Stop scheduling rather than failing: work already imported is
			// good, and the remainder keeps its stale hash so it retries.
			log.Warn("run budget exhausted; remaining units stay stale",
				"spent", spent, "budget", p.Budget.RunUSD)
			res.Summary.Status = StatusOverBudget
			break
		}

		unit := units[string(key)]
		item := ItemSummary{Key: key, Status: StatusPending}

		pagesOut, cost, turns, violations, err := p.generateUnit(ctx, req, key, unit, steering, known)
		spent += cost
		item.CostUSD = cost
		item.Turns = turns

		switch {
		case err != nil:
			item.Status = StatusFailed
			item.Err = err.Error()
			log.Error("unit failed", "key", key, "err", err)
		case len(violations) > 0:
			item.Status = StatusFailed
			item.Err = fmt.Sprintf("%d validation violations", len(violations))
			res.Violations = append(res.Violations, violations...)
			log.Error("unit failed validation", "key", key, "violations", len(violations))
		default:
			item.Status = StatusSucceeded
			written = append(written, pagesOut...)

			// The source record is only updated on success, so a failed unit
			// keeps its old hash and is retried on the next run.
			newSources = append(newSources, diff.SourceRecord{
				Key:          key,
				InputHash:    unit.Hash,
				FilesWritten: pagePaths(pagesOut),
			})
			for _, pg := range pagesOut {
				known[pg.Slug] = true
			}
		}

		res.Summary.Items = append(res.Summary.Items, item)
	}

	res.Summary.CostUSD = spent

	// Post-pass: rebuild the derived artifacts from what pages now exist. The
	// agent never writes these, so they cannot drift from the content.
	merged := mergePages(pages, written, cascade.DeletePages)
	index := wiki.BuildIndex(merged, wiki.IndexOptions{})

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

	if err := p.Store.Import(ctx, ImportRequest{
		WorkspaceID:     req.WorkspaceID,
		RunID:           req.RunID,
		UpsertPages:     written,
		SoftDeletePages: cascade.DeletePages,
		UpsertSources:   newSources,
		DropSources:     cascade.DropSources,
		Index:           index,
		LogEntry:        logEntry,
	}); err != nil {
		return nil, fmt.Errorf("jobs: import: %w", err)
	}

	if res.Summary.Status == StatusRunning {
		res.Summary.Status = runStatus(res.Summary.Items)
	}
	return res, p.Store.RecordRun(ctx, res.Summary)
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
		unit, ok := units[string(k)]
		// Units with no hash (the architecture synthesis) always regenerate
		// when routed, since their input is the whole map.
		if !ok || unit.Hash == "" || prior[k] != unit.Hash {
			out = append(out, k)
		}
	}
	return out
}

// generateUnit runs analyze then generate for one unit, retrying on validation
// failure with the errors stated back to the agent.
func (p *Pipeline) generateUnit(
	ctx context.Context,
	req BuildRequest,
	key diff.Key,
	unit mapper.Unit,
	steering Steering,
	known map[string]bool,
) (pages []wiki.Page, cost float64, turns int, violations []wiki.Violation, err error) {

	scratch := filepath.Join(req.ScratchDir, sanitize(string(key)))
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return nil, 0, 0, nil, fmt.Errorf("create scratch dir: %w", err)
	}

	sessionID := req.RunID + "-" + sanitize(string(key))
	attempts := p.MaxRetries + 1
	var lastViolations []wiki.Violation

	for attempt := range attempts {
		analyzeRes, aerr := p.Runner.Run(ctx, agent.Request{
			Step: agent.StepAnalyze, SessionID: sessionID,
			WorkDir: req.SourceDir, Model: p.AnalyzeModel,
			BudgetUSD: p.Budget.AnalyzeUSD, Timeout: p.Timeout,
			SystemPrompt: analyzeSystemPrompt(steering),
			Prompt:       analyzePrompt(key, unit, steering, attempt, lastViolations),
			JSONSchema:   AnalysisSchema,
		})
		if aerr != nil {
			return nil, cost, turns, nil, aerr
		}
		cost += analyzeRes.TotalCostUSD
		turns += analyzeRes.NumTurns
		if err := analyzeRes.Err(); err != nil {
			return nil, cost, turns, nil, err
		}

		genRes, gerr := p.Runner.Run(ctx, agent.Request{
			Step: agent.StepGenerate, SessionID: sessionID,
			WorkDir: req.SourceDir, ScratchDir: scratch,
			Model: p.modelFor(attempt), FallbackModel: p.FallbackModel,
			BudgetUSD: p.Budget.PageUSD, Timeout: p.Timeout,
			SystemPrompt: generateSystemPrompt(steering),
			Prompt:       generatePrompt(key, unit, steering, attempt, lastViolations),
		})
		if gerr != nil {
			return nil, cost, turns, nil, gerr
		}
		cost += genRes.TotalCostUSD
		turns += genRes.NumTurns
		if err := genRes.Err(); err != nil {
			return nil, cost, turns, nil, err
		}

		collected, cerr := collectPages(scratch)
		if cerr != nil {
			return nil, cost, turns, nil, cerr
		}

		lastViolations = wiki.ValidateBatch(collected, wiki.ValidateOptions{
			KnownSlugs:   known,
			RequireDates: false,
		})
		if len(lastViolations) == 0 {
			return derefPages(collected), cost, turns, nil, nil
		}

		p.logger().Warn("validation failed; retrying",
			"key", key, "attempt", attempt+1, "violations", len(lastViolations))
		// Clear the scratch dir so a partial attempt is not re-collected.
		if err := os.RemoveAll(scratch); err != nil {
			return nil, cost, turns, lastViolations, err
		}
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			return nil, cost, turns, lastViolations, err
		}
	}

	return nil, cost, turns, lastViolations, nil
}

// modelFor escalates one tier on the final attempt rather than starting
// expensive.
func (p *Pipeline) modelFor(attempt int) string {
	if attempt > 0 && p.FallbackModel != "" && attempt >= p.MaxRetries {
		return p.Model
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
