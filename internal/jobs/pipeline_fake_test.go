package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// These are the fake runner's contract tests: its output must survive every
// validation gate the real runners face, because it exists precisely so local
// end-to-end runs exercise the genuine pipeline.

func fakeTestMap() *mapper.WorkspaceMap {
	m := testMap(
		mapper.Unit{Key: "module:ripple", Kind: "module", Title: "Ripple",
			Dir: "apps/ripple", Hash: "h1"},
		mapper.Unit{Key: "module:pond", Kind: "module", Title: "Pond",
			Dir: "apps/pond", Hash: "h2"},
	)
	// The map hash gates the architecture synthesis; without it every build
	// would regenerate arch:overview and no repeat build could be free.
	m.Hash = "map-hash-1"
	return m
}

func fakeTestRequest(t *testing.T, m *mapper.WorkspaceMap) BuildRequest {
	t.Helper()
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.Router.ModuleDirs["apps/pond"] = diff.ModuleKey("pond")
	return req
}

func TestFakeRunnerSurvivesTheFullPipeline(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{CostPerCall: 0.01}))

	res, err := p.Build(context.Background(), fakeTestRequest(t, fakeTestMap()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("status = %s, want succeeded (violations: %v)", res.Summary.Status, res.Violations)
	}
	if len(res.Violations) != 0 {
		t.Errorf("violations: %v", res.Violations)
	}

	// Three units (two modules + the arch synthesis), two calls each
	// (analyze + generate): synthetic cost adds up.
	if got, want := res.Summary.CostUSD, 0.06; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing imported")
	}
	if len(imp.UpsertPages) != 3 {
		t.Fatalf("pages imported = %d, want one per unit incl. arch", len(imp.UpsertPages))
	}
	for _, pg := range imp.UpsertPages {
		if pg.Meta.Created == "" || pg.Meta.Updated == "" {
			t.Errorf("page %s missing stamped dates", pg.Path)
		}
		if !strings.Contains(pg.Body, "Content fingerprint:") {
			t.Errorf("page %s body missing fingerprint", pg.Path)
		}
	}
	// The index and log are derived exactly as in production.
	if imp.Index == "" || imp.LogEntry == "" {
		t.Error("derived artifacts missing")
	}
}

func TestFakeRunnerRepeatBuildHitsTheHashGate(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{}))
	m := fakeTestMap()

	if _, err := p.Build(context.Background(), fakeTestRequest(t, m)); err != nil {
		t.Fatal(err)
	}
	res, err := p.Build(context.Background(), fakeTestRequest(t, m))
	if err != nil {
		t.Fatal(err)
	}
	// The sources the fake produced round-trip through the store with hashes
	// intact, so an unchanged workspace costs nothing -- the property the
	// whole design leans on, now provable locally.
	if res.Summary.Status != StatusNoChanges {
		t.Errorf("repeat build = %s, want no_changes", res.Summary.Status)
	}
	if store.importCount() != 1 {
		t.Errorf("imports = %d, want 1", store.importCount())
	}
}

func TestFakeRunnerFailureInjectionYieldsPartialRun(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{
		FailUnits: []string{"module:pond"},
	}))

	res, err := p.Build(context.Background(), fakeTestRequest(t, fakeTestMap()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Status != StatusPartial {
		t.Fatalf("status = %s, want partial", res.Summary.Status)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("succeeded unit not imported")
	}
	for _, src := range imp.UpsertSources {
		if src.Key == diff.ModuleKey("pond") {
			// A failed unit must keep its stale hash so the next run retries it.
			t.Error("failed unit's source record was written")
		}
	}
	var sawFailed bool
	for _, item := range res.Summary.Items {
		if item.Key == diff.ModuleKey("pond") && item.Status == StatusFailed &&
			strings.Contains(item.Err, "injected failure") {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("failed item not reported: %+v", res.Summary.Items)
	}
}
