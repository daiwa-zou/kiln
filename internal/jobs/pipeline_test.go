package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// scriptedRunner is an in-process agent.Runner. The exec path itself is covered
// by the fake binary tests in internal/agent; here the concern is pipeline
// orchestration, so responses are scripted directly.
type scriptedRunner struct {
	mu sync.Mutex

	calls []agent.Request
	// files written per generate call, keyed by attempt number so a retry can
	// produce different output from the first try.
	filesByAttempt map[int]map[string]string
	attempts       map[string]int
	costPerCall    float64
	failWith       error
	errEnvelope    bool
}

func newScriptedRunner() *scriptedRunner {
	return &scriptedRunner{
		filesByAttempt: map[int]map[string]string{},
		attempts:       map[string]int{},
		costPerCall:    0.01,
	}
}

func (s *scriptedRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, req)

	if s.failWith != nil {
		return nil, s.failWith
	}
	if s.errEnvelope {
		return &agent.Result{IsError: true, Subtype: "error_during_execution", Result: "scripted failure"}, nil
	}

	res := &agent.Result{
		Subtype: "success", TerminalReason: "completed",
		SessionID: req.SessionID, NumTurns: 1, TotalCostUSD: s.costPerCall,
	}

	if req.Step == agent.StepGenerate {
		attempt := s.attempts[req.SessionID]
		s.attempts[req.SessionID] = attempt + 1
		if files, ok := s.filesByAttempt[attempt]; ok {
			if err := writeFiles(req.ScratchDir, files); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (s *scriptedRunner) callsFor(step agent.Step) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for _, c := range s.calls {
		if c.Step == step {
			n++
		}
	}
	return n
}

// generateCallsForUnit counts generate calls for one unit. Session IDs embed
// the cache key, so this isolates a single unit's retry behavior from the other
// units a run touches.
func (s *scriptedRunner) generateCallsForUnit(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := sanitize(key)
	var n int
	for _, c := range s.calls {
		if c.Step == agent.StepGenerate && strings.HasSuffix(c.SessionID, want) {
			n++
		}
	}
	return n
}

func writeFiles(dir string, files map[string]string) error {
	for rel, content := range files {
		if err := writeFile(dir, rel, content); err != nil {
			return err
		}
	}
	return nil
}

// validPage is long enough to clear the minimum body size.
func validPage(pageType, title string) string {
	return "---\ntype: " + pageType + "\ntitle: " + title +
		"\ncreated: 2026-07-25\nupdated: 2026-07-25\n---\n\n# " + title +
		"\n\nThis module coordinates work across the services, dispatching tasks " +
		"to workers and collecting their results for downstream consumers.\n"
}

func testPipeline(store Store, runner agent.Runner) *Pipeline {
	return &Pipeline{
		Store: store, Runner: runner,
		Budget:  Budget{AnalyzeUSD: 0.4, PageUSD: 1.5, RunUSD: 6, MaxPages: 12},
		Model:   "sonnet",
		Timeout: 30 * time.Second,
		Now:     func() time.Time { return time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC) },
	}
}

func testMap(units ...mapper.Unit) *mapper.WorkspaceMap {
	return &mapper.WorkspaceMap{Kind: "git", Units: units}
}

func testRequest(t *testing.T, m *mapper.WorkspaceMap, changes diff.ChangeSet) BuildRequest {
	t.Helper()
	return BuildRequest{
		RunID: "run-1", WorkspaceID: "ws-1", Trigger: "manual", Ref: "abc1234",
		SourceDir: t.TempDir(), ScratchDir: t.TempDir(),
		Map: m,
		Router: diff.Router{ModuleDirs: map[string]diff.Key{
			"apps/ripple": diff.ModuleKey("ripple"),
		}},
		Changes: changes,
	}
}

func TestBuildNoChangesCostsNothing(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	p := testPipeline(store, runner)

	req := testRequest(t, testMap(), diff.ChangeSet{})

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// This is the single most important behavior in the system: an unchanged
	// workspace must not invoke the agent at all.
	if len(runner.calls) != 0 {
		t.Errorf("agent was invoked %d times for an empty change set", len(runner.calls))
	}
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusNoChanges)
	}
	if res.Summary.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", res.Summary.CostUSD)
	}
}

func TestBuildSkipsUnchangedHash(t *testing.T) {
	store := newMemStore()
	// The unit was already built at this exact hash.
	store.sources[diff.ModuleKey("ripple")] = diff.SourceRecord{
		Key: diff.ModuleKey("ripple"), InputHash: "hash-v1",
		FilesWritten: []string{"entities/ripple.md"},
	}

	runner := newScriptedRunner()
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "hash-v1"})
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
	}})

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A file changed, but the unit hashed the same, so regeneration is skipped.
	if len(runner.calls) != 0 {
		t.Errorf("agent invoked despite an unchanged unit hash")
	}
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusNoChanges)
	}
}

func TestBuildRegeneratesChangedHash(t *testing.T) {
	store := newMemStore()
	store.sources[diff.ModuleKey("ripple")] = diff.SourceRecord{
		Key: diff.ModuleKey("ripple"), InputHash: "hash-v1",
	}

	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "hash-v2"})
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
	}})

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (violations: %v)", res.Summary.Status, res.Violations)
	}
	if runner.callsFor(agent.StepAnalyze) != 1 || runner.callsFor(agent.StepGenerate) != 1 {
		t.Errorf("want one analyze and one generate, got %d and %d",
			runner.callsFor(agent.StepAnalyze), runner.callsFor(agent.StepGenerate))
	}

	imp := store.lastImport()
	if imp == nil || len(imp.UpsertPages) != 1 {
		t.Fatalf("expected one imported page, got %+v", imp)
	}
	if imp.UpsertPages[0].Path != "entities/ripple.md" {
		t.Errorf("imported %q", imp.UpsertPages[0].Path)
	}

	// The source record must record the new hash, or the next run repeats work.
	if len(imp.UpsertSources) != 1 || imp.UpsertSources[0].InputHash != "hash-v2" {
		t.Errorf("source record = %+v, want hash-v2", imp.UpsertSources)
	}
}

func TestBuildResumesSessionAcrossSteps(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Both steps must share a session so the source context stays prompt-cached
	// between analyze and generate.
	var analyzeID, generateID string
	for _, c := range runner.calls {
		switch c.Step {
		case agent.StepAnalyze:
			analyzeID = c.SessionID
		case agent.StepGenerate:
			generateID = c.SessionID
		}
	}
	if analyzeID == "" || analyzeID != generateID {
		t.Errorf("session IDs differ: analyze=%q generate=%q", analyzeID, generateID)
	}
}

func TestBuildDryRunSpendsNothing(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	p := testPipeline(store, runner)

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.DryRun = true

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The estimate-and-confirm preview must make zero LLM calls.
	if len(runner.calls) != 0 {
		t.Errorf("dry run invoked the agent %d times", len(runner.calls))
	}
	if len(res.Planned) == 0 {
		t.Error("dry run reported no planned work")
	}
	if res.EstimatedUSD <= 0 {
		t.Error("dry run produced no cost estimate")
	}
	if store.importCount() != 0 {
		t.Error("dry run wrote to the store")
	}
}

func TestBuildRetriesOnValidationFailure(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	// First attempt writes a page with no frontmatter; the retry fixes it.
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": "# No frontmatter here\n",
	}
	runner.filesByAttempt[1] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}

	p := testPipeline(store, runner)
	p.MaxRetries = 1

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.Status != StatusSucceeded {
		t.Errorf("Status = %q, want succeeded after a corrective retry", res.Summary.Status)
	}

	// A full rebuild dirties the module and the architecture synthesis, so
	// count per unit rather than in total.
	if got := runner.generateCallsForUnit("module:ripple"); got != 2 {
		t.Errorf("generate calls for module:ripple = %d, want 2 (initial plus one retry)", got)
	}
}

func TestBuildRetryStatesViolationsBack(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{"entities/ripple.md": "# no frontmatter\n"}
	runner.filesByAttempt[1] = map[string]string{"entities/ripple.md": validPage("entity", "Ripple")}

	p := testPipeline(store, runner)
	p.MaxRetries = 1

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	if _, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true})); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The corrective turn has to say what was wrong, or the retry is a coin flip.
	var retryPrompt string
	for _, c := range runner.calls {
		if c.Step == agent.StepGenerate && strings.Contains(c.Prompt, "failed validation") {
			retryPrompt = c.Prompt
		}
	}
	if retryPrompt == "" {
		t.Fatal("retry prompt did not restate the validation failures")
	}
	if !strings.Contains(retryPrompt, "entities/ripple.md") {
		t.Errorf("retry prompt should name the offending file:\n%s", retryPrompt)
	}
}

func TestBuildFailsUnitAfterRetriesExhausted(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	// Never produces a valid page.
	for i := range 5 {
		runner.filesByAttempt[i] = map[string]string{"entities/ripple.md": "# bad\n"}
	}

	p := testPipeline(store, runner)
	p.MaxRetries = 1

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.Status != StatusFailed {
		t.Errorf("Status = %q, want failed", res.Summary.Status)
	}
	if len(res.Violations) == 0 {
		t.Error("no violations reported for a persistently invalid page")
	}

	// A failed unit must not record a source hash, or it would never retry.
	imp := store.lastImport()
	if imp != nil && len(imp.UpsertSources) != 0 {
		t.Errorf("failed unit recorded a source hash: %+v", imp.UpsertSources)
	}
}

func TestBuildDoesNotImportInvalidPages(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
		// Frontmatter says concept but it is filed under entities.
		"entities/wrong-type.md": validPage("concept", "Wrong"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The batch is rejected as a whole; nothing invalid reaches the store.
	if res.Summary.Status == StatusSucceeded {
		t.Error("a batch containing an invalid page was accepted")
	}
	for _, imp := range store.imports {
		for _, pg := range imp.UpsertPages {
			if pg.Path == "entities/wrong-type.md" {
				t.Error("an invalid page was imported")
			}
		}
	}
}

func TestBuildRejectsReservedPageWrites(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"index.md": "---\ntype: entity\ntitle: Hijacked\n---\n\n# Hijacked\n" + strings.Repeat("x ", 80),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// index.md is derived from frontmatter; an agent writing it would let
	// navigation drift from content.
	if res.Summary.Status == StatusSucceeded {
		t.Error("an agent-written index.md was accepted")
	}
}

func TestBuildRebuildsIndexDeterministically(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md":   validPage("entity", "Ripple"),
		"concepts/dispatch.md": validPage("concept", "Dispatch"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	if _, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true})); err != nil {
		t.Fatalf("Build: %v", err)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing was imported")
	}
	if !strings.Contains(imp.Index, "## Entities") || !strings.Contains(imp.Index, "## Concepts") {
		t.Errorf("index missing sections:\n%s", imp.Index)
	}
	if !strings.Contains(imp.Index, "[[entities/ripple|Ripple]]") {
		t.Errorf("index missing the entity entry:\n%s", imp.Index)
	}
	if !strings.Contains(imp.LogEntry, "## [2026-07-25] build |") {
		t.Errorf("log entry has the wrong shape:\n%s", imp.LogEntry)
	}
}

func TestBuildStopsAtRunBudget(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.costPerCall = 1.0 // two calls per unit, so ~2 USD each

	for i := range 10 {
		runner.filesByAttempt[i] = map[string]string{
			"entities/ripple.md": validPage("entity", "Ripple"),
		}
	}

	p := testPipeline(store, runner)
	p.Budget.RunUSD = 3.0

	m := testMap(
		mapper.Unit{Key: "module:a", Slug: "a", Hash: "h"},
		mapper.Unit{Key: "module:b", Slug: "b", Hash: "h"},
		mapper.Unit{Key: "module:c", Slug: "c", Hash: "h"},
		mapper.Unit{Key: "module:d", Slug: "d", Hash: "h"},
	)
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.Router = diff.Router{ModuleDirs: map[string]diff.Key{
		"a": diff.ModuleKey("a"), "b": diff.ModuleKey("b"),
		"c": diff.ModuleKey("c"), "d": diff.ModuleKey("d"),
	}}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.Status != StatusOverBudget {
		t.Errorf("Status = %q, want %q", res.Summary.Status, StatusOverBudget)
	}
	// Work already done is kept; the rest keeps its stale hash and retries.
	if len(res.Summary.Items) >= 4 {
		t.Errorf("budget did not stop scheduling: %d items ran", len(res.Summary.Items))
	}
	if store.importCount() == 0 {
		t.Error("partial work was discarded rather than imported")
	}
}

func TestBuildCapsPagesPerRun(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	p := testPipeline(store, runner)
	p.Budget.MaxPages = 2

	m := testMap(
		mapper.Unit{Key: "module:a", Slug: "a", Hash: "h"},
		mapper.Unit{Key: "module:b", Slug: "b", Hash: "h"},
		mapper.Unit{Key: "module:c", Slug: "c", Hash: "h"},
	)
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.Router = diff.Router{ModuleDirs: map[string]diff.Key{
		"a": diff.ModuleKey("a"), "b": diff.ModuleKey("b"), "c": diff.ModuleKey("c"),
	}}
	req.DryRun = true

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Planned) != 2 {
		t.Errorf("Planned = %d units, want the cap of 2", len(res.Planned))
	}
}

func TestBuildAgentTransportErrorFailsUnit(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.errEnvelope = true

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusFailed {
		t.Errorf("Status = %q, want failed", res.Summary.Status)
	}
}

func TestBuildInjectsSteering(t *testing.T) {
	store := newMemStore()
	store.steering = Steering{
		Purpose: "Document the dispatch subsystem for on-call engineers.",
		Schema:  "Entity pages are one per service.",
		Corrections: map[string][]string{
			"ripple": {"Ripple does not own retry policy; beacon does."},
		},
	}

	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	if _, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true})); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var sawPurpose, sawCorrection bool
	for _, c := range runner.calls {
		if strings.Contains(c.SystemPrompt, "on-call engineers") {
			sawPurpose = true
		}
		// Pinned corrections are what let human knowledge survive a rebuild.
		if strings.Contains(c.Prompt, "beacon does") {
			sawCorrection = true
		}
	}
	if !sawPurpose {
		t.Error("the purpose document was not injected into any prompt")
	}
	if !sawCorrection {
		t.Error("pinned corrections were not injected into any prompt")
	}
}

func TestBuildFramesSourcesAsUntrusted(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	if _, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true})); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// CLI flags stop source-supplied configuration loading; they cannot address
	// instructions embedded in file content, so the prompt must.
	for _, c := range runner.calls {
		if !strings.Contains(c.SystemPrompt, "UNTRUSTED DATA") {
			t.Errorf("%s prompt lacks untrusted-input framing", c.Step)
		}
	}
}

func TestBuildCascadeDeletesOnlyUnclaimedPages(t *testing.T) {
	store := newMemStore()
	store.sources[diff.ModuleKey("alpha")] = diff.SourceRecord{
		Key: diff.ModuleKey("alpha"), InputHash: "h",
		FilesWritten: []string{"entities/alpha.md", "concepts/shared.md"},
	}
	store.sources[diff.ModuleKey("beta")] = diff.SourceRecord{
		Key: diff.ModuleKey("beta"), InputHash: "h",
		FilesWritten: []string{"entities/beta.md", "concepts/shared.md"},
	}
	store.pages["entities/alpha.md"] = wiki.Page{Path: "entities/alpha.md", Slug: "alpha"}
	store.pages["concepts/shared.md"] = wiki.Page{Path: "concepts/shared.md", Slug: "shared"}

	runner := newScriptedRunner()
	p := testPipeline(store, runner)

	req := testRequest(t, testMap(), diff.ChangeSet{})
	req.ApprovedDeletions = []diff.Key{diff.ModuleKey("alpha")}

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("cascade produced no import")
	}
	if len(imp.SoftDeletePages) != 1 || imp.SoftDeletePages[0] != "entities/alpha.md" {
		t.Errorf("SoftDeletePages = %v, want only entities/alpha.md; the shared page must survive",
			imp.SoftDeletePages)
	}
}

func TestBuildIsIdempotent(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	for i := range 4 {
		runner.filesByAttempt[i] = map[string]string{
			"entities/ripple.md": validPage("entity", "Ripple"),
		}
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "stable-hash"})

	first := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	if _, err := p.Build(context.Background(), first); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	callsAfterFirst := len(runner.calls)

	// A second run over the same inputs must be free.
	second := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
	}})
	res, err := p.Build(context.Background(), second)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}

	if len(runner.calls) != callsAfterFirst {
		t.Errorf("second run made %d additional agent calls; it should make none",
			len(runner.calls)-callsAfterFirst)
	}
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("second run Status = %q, want %q", res.Summary.Status, StatusNoChanges)
	}
}
