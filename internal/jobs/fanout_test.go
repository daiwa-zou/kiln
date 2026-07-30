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
)

// concurrentRunner is structuredRunner made safe to call from many
// goroutines, plus the two things the fan-out tests need to assert on: the
// peak number of overlapping calls, which proves units really did run at the
// same time rather than the test passing on a sequential pipeline, and a
// per-unit delay for forcing a completion order that differs from plan order.
type concurrentRunner struct {
	mu     sync.Mutex
	byUnit map[string][]agent.GeneratedPage
	delays map[string]time.Duration
	// defaultDelay applies to units with no entry in delays. Some delay is
	// needed for calls to overlap at all: instant calls finish before the
	// scheduler reaches the next unit, and the test would pass against a
	// sequential pipeline.
	defaultDelay time.Duration
	cost         float64
	inFlight     int
	peak         int
	calls        int
}

func newConcurrentRunner(byUnit map[string][]agent.GeneratedPage) *concurrentRunner {
	return &concurrentRunner{byUnit: byUnit, delays: map[string]time.Duration{}, cost: 0.01}
}

func (r *concurrentRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	unit := r.unitFor(req.SessionID)

	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	delay, ok := r.delays[unit]
	if !ok {
		delay = r.defaultDelay
	}
	cost := r.cost
	r.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()

	res := &agent.Result{
		Subtype: "success", TerminalReason: "completed",
		SessionID: req.SessionID, NumTurns: 1, TotalCostUSD: cost,
	}
	if req.Step != agent.StepGenerate {
		return res, nil
	}
	r.mu.Lock()
	pages := r.byUnit[unit]
	r.mu.Unlock()
	res.Generation = &agent.GenerationResult{Pages: pages}
	return res, nil
}

// unitFor recovers the unit key from the session id, which embeds it
// sanitized.
func (r *concurrentRunner) unitFor(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for unit := range r.byUnit {
		if strings.HasSuffix(sessionID, sanitize(unit)) {
			return unit
		}
	}
	return ""
}

func (r *concurrentRunner) peakInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// body returns a page body that clears MinBodyBytes and carries a heading,
// with any wikilinks the caller wants appended.
func body(title string, links ...string) string {
	b := "# " + title + "\n\nThis unit coordinates work across the services, dispatching " +
		"tasks to workers and collecting their results for downstream consumers.\n"
	for _, l := range links {
		b += "\nSee [[" + l + "]] for the details of that part of the system.\n"
	}
	return b
}

func genPage(path, title string, links ...string) agent.GeneratedPage {
	return agent.GeneratedPage{Path: path, Type: "entity", Title: title, Body: body(title, links...)}
}

// genTypedPage is genPage for pages outside entities/. Validation requires the
// frontmatter type and the directory to agree, so a test that needs two paths
// sharing a basename needs the second one's type to match its directory.
func genTypedPage(path, pageType, title string) agent.GeneratedPage {
	return agent.GeneratedPage{Path: path, Type: pageType, Title: title, Body: body(title)}
}

// fanoutRequest builds a request whose router maps one module dir per unit,
// so every named unit is routed dirty by a full rebuild.
func fanoutRequest(t *testing.T, units ...mapper.Unit) BuildRequest {
	t.Helper()
	req := testRequest(t, testMap(units...), diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = "" // structured output; nothing touches disk
	dirs := map[string]diff.Key{}
	for _, u := range units {
		dirs["apps/"+u.Slug] = diff.Key(u.Key)
	}
	req.Router = diff.Router{ModuleDirs: dirs}
	return req
}

func TestBudgetLedgerHoldsCeilingUnderConcurrency(t *testing.T) {
	const (
		limit   = 10.0
		perCall = 1.0
		callers = 200
	)
	ledger := newBudgetLedger(limit)

	var wg sync.WaitGroup
	var granted int64
	var mu sync.Mutex
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !ledger.reserve(perCall) {
				return
			}
			mu.Lock()
			granted++
			mu.Unlock()
			ledger.settle(perCall, perCall)
		}()
	}
	wg.Wait()

	// The ceiling is the point: 200 callers racing for 10 units of budget
	// must settle at exactly 10, not at 10 times the concurrency.
	if got := ledger.totalSpent(); got > limit {
		t.Errorf("spent %.2f exceeds the %.2f ceiling", got, limit)
	}
	if granted != int64(limit/perCall) {
		t.Errorf("granted %d reservations, want %d", granted, int64(limit/perCall))
	}
}

func TestBudgetLedgerReleasesUnusedReservation(t *testing.T) {
	ledger := newBudgetLedger(1.0)

	// Reserve the whole ceiling, then settle for almost nothing: the
	// difference must return to the pool or a run would strand budget it
	// never spent.
	if !ledger.reserve(1.0) {
		t.Fatal("first reservation refused against an empty ledger")
	}
	ledger.settle(1.0, 0.01)

	if !ledger.reserve(0.9) {
		t.Error("headroom released by an underspent call was not reusable")
	}
	if ledger.reserve(0.5) {
		t.Error("reservation granted past the ceiling")
	}
}

// TestFanOutHoldsRunBudget is the regression this whole design exists for.
// The sequential pipeline bounded spend by checking an accumulator between
// units; with C units in flight that check would pass C times against one
// snapshot, and the ceiling would scale with concurrency.
func TestFanOutHoldsRunBudget(t *testing.T) {
	const runBudget = 0.20

	units := make([]mapper.Unit, 0, 20)
	pages := map[string][]agent.GeneratedPage{}
	for i := range 20 {
		slug := string(rune('a' + i))
		key := "module:" + slug
		units = append(units, mapper.Unit{Key: key, Slug: slug, Hash: "h" + slug})
		pages[key] = []agent.GeneratedPage{genPage("entities/"+slug+".md", "Unit "+slug)}
	}

	store := newMemStore()
	runner := newConcurrentRunner(pages)
	runner.cost = 0.01
	runner.defaultDelay = 5 * time.Millisecond

	p := testPipeline(store, runner)
	p.UnitConcurrency = 8
	p.Budget = Budget{AnalyzeUSD: 0.01, PageUSD: 0.01, RunUSD: runBudget, MaxPages: 100}

	res, err := p.Build(context.Background(), fanoutRequest(t, units...))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if res.Summary.CostUSD > runBudget+1e-9 {
		t.Errorf("spent $%.4f against a $%.2f ceiling; the budget did not hold under fan-out",
			res.Summary.CostUSD, runBudget)
	}
	if res.Summary.Status != StatusOverBudget {
		t.Errorf("Status = %q, want %q -- 20 units cannot fit this budget",
			res.Summary.Status, StatusOverBudget)
	}
	if peak := runner.peakInFlight(); peak < 2 {
		t.Errorf("peak in-flight calls = %d; the run never actually fanned out, so this proves nothing", peak)
	}
}

// TestFanOutRejectsCrossUnitSlugCollision covers what the snapshot gave up.
// The sequential pipeline caught two units claiming one slug because it grew
// existingBySlug as each landed; concurrent units all read the same snapshot,
// so the check has to be explicit in the merge.
func TestFanOutRejectsCrossUnitSlugCollision(t *testing.T) {
	store := newMemStore()
	// Different paths, same basename -- so the same slug, which is the
	// database's page identity. The second import would silently overwrite
	// the first row.
	runner := newConcurrentRunner(map[string][]agent.GeneratedPage{
		"module:alpha": {genPage("entities/shared.md", "Shared")},
		"module:beta":  {genTypedPage("concepts/shared.md", "concept", "Shared")},
	})

	p := testPipeline(store, runner)
	p.UnitConcurrency = 4

	res, err := p.Build(context.Background(), fanoutRequest(t,
		mapper.Unit{Key: "module:alpha", Slug: "alpha", Hash: "h1"},
		mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"},
	))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A full rebuild also plans arch:overview, which writes nothing here;
	// only the two colliding module units are under test.
	var succeeded, failed int
	for _, it := range res.Summary.Items {
		if !strings.HasPrefix(string(it.Key), "module:") {
			continue
		}
		switch it.Status {
		case StatusSucceeded:
			succeeded++
		case StatusFailed:
			failed++
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("got %d succeeded / %d failed module units, want exactly one of each", succeeded, failed)
	}

	var mentionsSlug bool
	for _, v := range res.Violations {
		if strings.Contains(v.Reason, "slug") {
			mentionsSlug = true
		}
	}
	if !mentionsSlug {
		t.Errorf("no violation named the slug collision: %+v", res.Violations)
	}

	// Exactly one page reaches the database -- the collision must be caught
	// before import, not resolved by whichever upsert happened to run last.
	if imp := store.lastImport(); imp == nil || len(imp.UpsertPages) != 1 {
		t.Errorf("imported %v pages, want 1", imp)
	}
}

// TestFanOutResolvesLinksAcrossUnits is the correctness gain, not just a
// concession to concurrency: in the sequential pipeline a unit could only
// link to pages written by units before it, so an early unit linking to a
// later one's page validated as dangling.
func TestFanOutResolvesLinksAcrossUnits(t *testing.T) {
	// Four links, one over DefaultMaxUnresolvedLinks: if the check ran
	// against the pre-run snapshot this unit would fail outright.
	store := newMemStore()
	runner := newConcurrentRunner(map[string][]agent.GeneratedPage{
		"module:alpha": {genPage("entities/alpha.md", "Alpha", "b1", "b2", "b3", "b4")},
		"module:beta": {
			genPage("entities/b1.md", "B1"), genPage("entities/b2.md", "B2"),
			genPage("entities/b3.md", "B3"), genPage("entities/b4.md", "B4"),
		},
	})
	// Alpha finishes first, so its links point at pages that do not exist
	// yet at the moment it is validated.
	runner.delays["module:beta"] = 20 * time.Millisecond

	p := testPipeline(store, runner)
	p.UnitConcurrency = 2

	res, err := p.Build(context.Background(), fanoutRequest(t,
		mapper.Unit{Key: "module:alpha", Slug: "alpha", Hash: "h1"},
		mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"},
	))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, it := range res.Summary.Items {
		if it.Status != StatusSucceeded {
			t.Errorf("unit %s = %s (%s); links across concurrent units must resolve",
				it.Key, it.Status, it.Err)
		}
	}
	if len(res.Violations) != 0 {
		t.Errorf("violations on a clean cross-linked run: %+v", res.Violations)
	}
}

// TestFanOutRecordsFollowPlanOrder pins determinism. Two runs over identical
// inputs must produce identical records, so the order units happen to finish
// in cannot be allowed to leak into the summary or the import.
func TestFanOutRecordsFollowPlanOrder(t *testing.T) {
	units := []mapper.Unit{
		{Key: "module:alpha", Slug: "alpha", Hash: "h1"},
		{Key: "module:beta", Slug: "beta", Hash: "h2"},
		{Key: "module:gamma", Slug: "gamma", Hash: "h3"},
	}
	pages := map[string][]agent.GeneratedPage{
		"module:alpha": {genPage("entities/alpha.md", "Alpha")},
		"module:beta":  {genPage("entities/beta.md", "Beta")},
		"module:gamma": {genPage("entities/gamma.md", "Gamma")},
	}

	store := newMemStore()
	runner := newConcurrentRunner(pages)
	// Finish in reverse: the first unit in plan order completes last.
	runner.delays["module:alpha"] = 30 * time.Millisecond
	runner.delays["module:beta"] = 15 * time.Millisecond

	p := testPipeline(store, runner)
	p.UnitConcurrency = 3

	res, err := p.Build(context.Background(), fanoutRequest(t, units...))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// arch:overview is planned last on a full rebuild and belongs in the
	// expectation: plan order is the whole point of this test.
	want := []string{"module:alpha", "module:beta", "module:gamma", "arch:overview"}
	if len(res.Summary.Items) != len(want) {
		t.Fatalf("got %d items, want %d", len(res.Summary.Items), len(want))
	}
	for i, w := range want {
		if got := string(res.Summary.Items[i].Key); got != w {
			t.Errorf("item %d = %s, want %s (completion order leaked into the record)", i, got, w)
		}
	}
	if peak := runner.peakInFlight(); peak < 2 {
		t.Errorf("peak in-flight = %d; the units never overlapped", peak)
	}
}
