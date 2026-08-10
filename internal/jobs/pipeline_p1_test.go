package jobs

// Tests for the correctness fixes from the 2026-07 review: model escalation,
// the arch-overview hash gate, the analysis handoff, plan quarantine, date
// stamping, slug-collision rejection, failed-import ledgering, cancellation,
// the unknown-model budget guard, parse-error violations, and cascade
// regeneration.

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func TestModelForEscalatesOnFinalAttempt(t *testing.T) {
	p := &Pipeline{Model: "sonnet", FallbackModel: "opus", MaxRetries: 1}

	if got := p.modelFor(0); got != "sonnet" {
		t.Errorf("first attempt model = %q, want sonnet", got)
	}
	// The final corrective attempt escalates rather than repeating the model
	// that just failed validation.
	if got := p.modelFor(1); got != "opus" {
		t.Errorf("final attempt model = %q, want opus", got)
	}

	// No fallback configured: never escalate.
	p.FallbackModel = ""
	if got := p.modelFor(1); got != "sonnet" {
		t.Errorf("final attempt without fallback = %q, want sonnet", got)
	}
}

func TestBuildFullRebuildRepeatCostsNothing(t *testing.T) {
	// The CLI path always sends FullRebuild: true, so this is the invariant
	// the README sells: a second build over unchanged sources makes zero
	// agent calls -- including the architecture synthesis, which is gated by
	// the whole-map hash.
	store := newMemStore()
	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
			Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
		}},
		string(diff.ArchOverview): {{
			Path: "synthesis/architecture.md", Type: "synthesis", Title: "Architecture",
			Body: "# Architecture\n\n" + strings.Repeat("One binary, two roles, one queue. ", 8),
		}},
	}}
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	m.Hash = "map-hash-1"

	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	firstCalls := runner.calls
	if firstCalls == 0 {
		t.Fatal("first build made no agent calls")
	}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if runner.calls != firstCalls {
		t.Errorf("second build over unchanged sources made %d agent calls, want 0",
			runner.calls-firstCalls)
	}
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusNoChanges)
	}
}

func TestBuildForceSkipsHashGate(t *testing.T) {
	store := newMemStore()
	store.sources[diff.ModuleKey("ripple")] = diff.SourceRecord{
		Key: diff.ModuleKey("ripple"), InputHash: "h1",
		FilesWritten: []string{"entities/ripple.md"},
	}

	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
	}})
	req.Force = true

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The hash matches, but --full asked for regeneration anyway.
	if runner.callsFor(agent.StepGenerate) == 0 {
		t.Error("force build skipped an unchanged unit; --full must regenerate it")
	}
}

// planningRunner returns a structured analysis and then a page matching it,
// recording the generate prompt so tests can assert the plan reached it.
type planningRunner struct {
	generatePrompt string
}

func (r *planningRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	res := &agent.Result{
		Subtype: "success", TerminalReason: "completed",
		SessionID: req.SessionID, NumTurns: 1, TotalCostUSD: 0.01,
	}
	switch req.Step {
	case agent.StepAnalyze:
		res.Analysis = &agent.AnalysisResult{Pages: []agent.AnalysisPage{{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple", Summary: "dispatcher",
		}}}
	case agent.StepGenerate:
		r.generatePrompt = req.Prompt
		res.Generation = &agent.GenerationResult{Pages: []agent.GeneratedPage{{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
			Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
		}}}
	}
	return res, nil
}

func TestBuildPassesAnalysisPlanToGenerate(t *testing.T) {
	// On the stateless API runner the analyze output only reaches generation
	// if the pipeline hands it over explicitly. Paying for a plan and then
	// discarding it was the single most expensive bug in the original design.
	store := newMemStore()
	runner := &planningRunner{}
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if runner.generatePrompt == "" {
		t.Fatal("no generate call recorded")
	}
	if !strings.Contains(runner.generatePrompt, "plan from your analysis") {
		t.Error("generate prompt has no plan section")
	}
	if !strings.Contains(runner.generatePrompt, "entities/ripple.md") {
		t.Errorf("planned path missing from generate prompt:\n%s", runner.generatePrompt)
	}
}

type rogueRunner struct{}

func (r *rogueRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	res := &agent.Result{
		Subtype: "success", TerminalReason: "completed",
		SessionID: req.SessionID, NumTurns: 1, TotalCostUSD: 0.01,
	}
	switch req.Step {
	case agent.StepAnalyze:
		res.Analysis = &agent.AnalysisResult{Pages: []agent.AnalysisPage{{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple", Summary: "dispatcher",
		}}}
	case agent.StepGenerate:
		res.Generation = &agent.GenerationResult{Pages: []agent.GeneratedPage{
			{
				Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
				Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
			},
			{
				Path: "concepts/surprise.md", Type: "concept", Title: "Surprise",
				Body: "# Surprise\n\n" + strings.Repeat("Nobody asked for this page to exist. ", 8),
			},
		}}
	}
	return res, nil
}

func TestBuildQuarantinesPagesOutsideThePlan(t *testing.T) {
	// The analysis authorized one page; generation returned a second. The
	// unplanned page must fail validation rather than land silently.
	store := newMemStore()
	runner := &rogueRunner{}
	p := testPipeline(store, runner)
	p.MaxRetries = 0

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var quarantined bool
	for _, v := range res.Violations {
		if v.Path == "concepts/surprise.md" && strings.Contains(v.Reason, "not in the approved plan") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Errorf("unplanned page was not quarantined; violations: %v", res.Violations)
	}
}

func TestBuildStampsDates(t *testing.T) {
	store := newMemStore()
	// A page that already exists keeps its original Created.
	store.pages["entities/ripple.md"] = wiki.Page{
		Path: "entities/ripple.md", Slug: "ripple",
		Meta: wiki.Frontmatter{Type: wiki.TypeEntity, Title: "Ripple", Created: "2024-01-01"},
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
			Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
		}},
	}}
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h2"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	imp := store.lastImport()
	if imp == nil || len(imp.UpsertPages) == 0 {
		t.Fatal("nothing imported")
	}
	pg := imp.UpsertPages[0]
	if pg.Meta.Created != "2024-01-01" {
		t.Errorf("Created = %q, want the original 2024-01-01 preserved", pg.Meta.Created)
	}
	if pg.Meta.Updated != "2026-07-25" {
		t.Errorf("Updated = %q, want the pinned run date", pg.Meta.Updated)
	}
}

func TestBuildRejectsSlugCollisionAcrossDirectories(t *testing.T) {
	// The database keys pages by slug. Two paths sharing a basename would
	// silently collapse into one row on import, so validation must refuse.
	store := newMemStore()
	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {
			{
				Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
				Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
			},
			{
				Path: "concepts/ripple.md", Type: "concept", Title: "Rippling",
				Body: "# Rippling\n\n" + strings.Repeat("The idea of work spreading outward. ", 8),
			},
		},
	}}
	p := testPipeline(store, runner)
	p.MaxRetries = 0

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var found bool
	for _, v := range res.Violations {
		if strings.Contains(v.Reason, "collides") {
			found = true
		}
	}
	if !found {
		t.Errorf("slug collision not rejected; violations: %v", res.Violations)
	}
	if imp := store.lastImport(); imp != nil && len(imp.UpsertPages) > 0 {
		t.Error("colliding pages were imported anyway")
	}
}

func TestBuildLedgersRunWhenImportFails(t *testing.T) {
	// The agent calls are already billed by the time import runs; a run that
	// fails at import must still land in the ledger or budget windows
	// undercount forever.
	store := newMemStore()
	store.failImport = context.DeadlineExceeded

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
			Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
		}},
	}}
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err == nil {
		t.Fatal("Build succeeded despite a failing import")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.runs) != 1 {
		t.Fatalf("recorded %d runs, want 1 failed run", len(store.runs))
	}
	run := store.runs[0]
	if run.Status != StatusFailed {
		t.Errorf("Status = %q, want %q", run.Status, StatusFailed)
	}
	if run.CostUSD == 0 {
		t.Error("failed run ledgered with zero cost despite billed agent calls")
	}
	if !strings.Contains(run.Err, "import") {
		t.Errorf("Err = %q, want the import failure recorded", run.Err)
	}
}

// cancelingRunner cancels the run's context after N complete units, simulating
// Ctrl-C between units.
type cancelingRunner struct {
	cancel          context.CancelFunc
	cancelAfterUnit int
	generates       int
}

func (r *cancelingRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	res := &agent.Result{
		Subtype: "success", TerminalReason: "completed",
		SessionID: req.SessionID, NumTurns: 1, TotalCostUSD: 0.01,
	}
	if req.Step == agent.StepGenerate {
		res.Generation = &agent.GenerationResult{Pages: []agent.GeneratedPage{{
			Path: "entities/unit-" + string(rune('a'+r.generates)) + ".md", Type: "entity",
			Title: "Unit", Body: "# Unit\n\n" + strings.Repeat("A module that exists to be counted. ", 8),
		}}}
		r.generates++
		if r.generates >= r.cancelAfterUnit {
			r.cancel()
		}
	}
	return res, nil
}

func TestBuildCanceledMidRunImportsCompletedWork(t *testing.T) {
	store := newMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &cancelingRunner{cancel: cancel, cancelAfterUnit: 1}
	p := testPipeline(store, runner)

	m := testMap(
		mapper.Unit{Key: "module:alpha", Slug: "alpha", Hash: "h1"},
		mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"},
	)
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""
	req.Router = diff.Router{ModuleDirs: map[string]diff.Key{
		"apps/alpha": diff.ModuleKey("alpha"),
		"apps/beta":  diff.ModuleKey("beta"),
	}}

	res, err := p.Build(ctx, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.Status != StatusCanceled {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusCanceled)
	}
	// Exactly one unit ran before cancellation; its output must be imported.
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("canceled run imported nothing; completed work was lost")
	}
	if len(imp.UpsertPages) != 1 {
		t.Errorf("imported %d pages, want the 1 completed before cancel", len(imp.UpsertPages))
	}
	if len(res.Summary.Items) != 1 {
		t.Errorf("ran %d units after cancellation, want 1", len(res.Summary.Items))
	}
}

func TestBuildBudgetGuardRejectsUnknownModel(t *testing.T) {
	// On the API runner, an unknown model estimates every call at $0, which
	// silently disables the run budget. Refusing to start is the only honest
	// behavior.
	store := newMemStore()
	p := testPipeline(store, agent.NewAPIRunner(agent.APIOptions{Model: "claude-invented-9"}))
	p.Model = "claude-invented-9"

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	_, err := p.Build(context.Background(), req)
	if err == nil {
		t.Fatal("Build started a budgeted run with an unpriceable model")
	}
	if !strings.Contains(err.Error(), "pricing") {
		t.Errorf("error should name the pricing gap, got: %v", err)
	}
}

func TestBuildParseFailureCarriesRealError(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	// First attempt writes a page with a broken frontmatter fence; the second
	// writes a valid one.
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": "---\ntype: entity\ntitle: Ripple\n\n# No closing fence\n",
	}
	runner.filesByAttempt[1] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}
	p := testPipeline(store, runner)
	p.MaxRetries = 1

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The corrective prompt must carry the parse error, not a misleading
	// "body is empty".
	var retryPrompt string
	runner.mu.Lock()
	for _, c := range runner.calls {
		if c.Step == agent.StepGenerate && strings.Contains(c.Prompt, "previous attempt failed") {
			retryPrompt = c.Prompt
		}
	}
	runner.mu.Unlock()

	if retryPrompt == "" {
		t.Fatal("no retry happened")
	}
	if !strings.Contains(retryPrompt, "failed to parse") {
		t.Errorf("retry prompt lacks the parse error:\n%s", retryPrompt)
	}
	if strings.Contains(retryPrompt, "body is empty") {
		t.Error("retry prompt carries the misleading empty-body diagnosis")
	}
}

func TestBuildRegeneratesCascadeSurvivors(t *testing.T) {
	// Two sources share a page. Deleting one keeps the page but must
	// regenerate it: its prose still describes the departed source.
	store := newMemStore()
	store.sources[diff.ModuleKey("alpha")] = diff.SourceRecord{
		Key: diff.ModuleKey("alpha"), InputHash: "h1",
		FilesWritten: []string{"concepts/shared.md"},
	}
	store.sources[diff.ModuleKey("beta")] = diff.SourceRecord{
		Key: diff.ModuleKey("beta"), InputHash: "h2",
		FilesWritten: []string{"concepts/shared.md", "entities/beta.md"},
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:beta": {{
			Path: "concepts/shared.md", Type: "concept", Title: "Shared",
			Body: "# Shared\n\n" + strings.Repeat("A concept that now derives from one source. ", 8),
		}},
	}}
	p := testPipeline(store, runner)

	// No content changes at all; only the approved deletion drives work.
	m := testMap(mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"})
	req := testRequest(t, m, diff.ChangeSet{})
	req.ScratchDir = ""
	req.ApprovedDeletions = []diff.Key{diff.ModuleKey("alpha")}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if runner.calls == 0 {
		t.Fatal("surviving unit was not regenerated after its sibling's deletion")
	}
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing imported")
	}
	if len(imp.UpsertPages) == 0 || imp.UpsertPages[0].Path != "concepts/shared.md" {
		t.Errorf("regenerated pages = %+v, want concepts/shared.md", imp.UpsertPages)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Errorf("Status = %q", res.Summary.Status)
	}
}

func TestBuildRegeneratesSurvivorsOfAnEarlierDeletion(t *testing.T) {
	// The same debt as TestBuildRegeneratesCascadeSurvivors, owed by a deletion
	// that already happened. An explicit source delete cascades at the moment
	// of the request, so there is no ApprovedDeletions to plan from -- only the
	// flag it left on the source that still claims the shared page.
	store := newMemStore()
	store.sources[diff.ModuleKey("beta")] = diff.SourceRecord{
		Key: diff.ModuleKey("beta"), InputHash: "h2",
		FilesWritten: []string{"concepts/shared.md", "entities/beta.md"},
		NeedsRegen:   true,
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:beta": {{
			Path: "concepts/shared.md", Type: "concept", Title: "Shared",
			Body: "# Shared\n\n" + strings.Repeat("A concept that now derives from one source. ", 8),
		}},
	}}
	p := testPipeline(store, runner)

	// Nothing changed and nothing is being deleted this run: the unit's hash
	// still matches, so the flag is the only thing that can schedule the work.
	m := testMap(mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"})
	req := testRequest(t, m, diff.ChangeSet{})
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if runner.calls == 0 {
		t.Fatal("flagged source was not regenerated; the shared page still describes a deleted source")
	}
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing imported")
	}
	if len(imp.UpsertPages) == 0 || imp.UpsertPages[0].Path != "concepts/shared.md" {
		t.Errorf("regenerated pages = %+v, want concepts/shared.md", imp.UpsertPages)
	}
}

func TestFlaggedSourceMissingFromTheMapIsNotScheduled(t *testing.T) {
	// A flagged source the current map does not contain cannot be regenerated:
	// its material is not in front of this run. Scheduling it anyway would ask
	// the agent to rewrite a page from a unit that does not exist, and the
	// build must instead leave the flag for a run that does map it.
	store := newMemStore()
	store.sources[diff.ModuleKey("beta")] = diff.SourceRecord{
		Key: diff.ModuleKey("beta"), InputHash: "h2",
		FilesWritten: []string{"concepts/shared.md"},
		NeedsRegen:   true,
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{}}
	p := testPipeline(store, runner)

	req := testRequest(t, testMap(), diff.ChangeSet{})
	req.ScratchDir = ""

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("ran %d agent calls for a unit that is not in the map", runner.calls)
	}
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusNoChanges)
	}
}
