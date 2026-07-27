package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// M6 content-correctness tests: the deletion blind spot, per-source
// connector attribution, and the cross-unit page collision guard.

func TestDeletionFlaggedForLastSourceOfAKind(t *testing.T) {
	store := newMemStore()
	// One uploaded document on record; the new map has none — the uploads
	// directory is now empty.
	store.sources[diff.DocKey(diff.UploadOrigin("notes.md"))] = diff.SourceRecord{
		Key: diff.DocKey(diff.UploadOrigin("notes.md")), InputHash: "h",
		FilesWritten: []string{"sources/notes.md"},
	}
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{}))

	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"})
	m.Hash = "map-1"
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	// Uploads were synced this run and came back empty: that IS a deletion,
	// the case the old prefix-inference guard was blind to.
	req.SyncedNamespaces = []string{"module", "entry", "arch", "doc", "doc:upload"}
	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.deletionReviews) != 1 ||
		store.deletionReviews[0].Key != diff.DocKey(diff.UploadOrigin("notes.md")) {
		t.Fatalf("deletion reviews = %+v, want the vanished upload", store.deletionReviews)
	}

	// A run that did not sync uploads must not read absence as deletion.
	store2 := newMemStore()
	store2.sources[diff.DocKey(diff.UploadOrigin("notes.md"))] = store.sources[diff.DocKey(diff.UploadOrigin("notes.md"))]
	p2 := testPipeline(store2, agent.NewFakeRunner(agent.FakeOptions{}))
	req2 := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req2.SyncedNamespaces = []string{"module", "entry", "arch", "doc"} // no doc:upload
	if _, err := p2.Build(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	if len(store2.deletionReviews) != 0 {
		t.Fatalf("unsynced namespace produced deletion reviews: %+v", store2.deletionReviews)
	}
}

func TestPerSourceConnectorAttribution(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{}))

	m := testMap(
		mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h1"},
		mapper.Unit{Key: "doc:" + diff.UploadOrigin("guide.md"), Kind: "doc", Hash: "h2"},
	)
	m.Hash = "map-1"
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.Connectors = SourceConnectors{Git: "conn-git", Upload: "conn-up", Web: "conn-web"}
	// The doc unit must be routable or no source record is written for it.
	req.Router.DocPaths = map[string]diff.Key{
		diff.UploadOrigin("guide.md"): diff.DocKey(diff.UploadOrigin("guide.md")),
	}

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing imported")
	}
	got := map[string]string{}
	for _, src := range imp.UpsertSources {
		got[string(src.Key)] = src.ConnectorID
	}
	if got["module:ripple"] != "conn-git" {
		t.Errorf("module attribution = %q, want conn-git", got["module:ripple"])
	}
	if got["doc:upload:guide.md"] != "conn-up" {
		t.Errorf("upload attribution = %q, want conn-up", got["doc:upload:guide.md"])
	}
	if got["arch:overview"] != "conn-git" {
		t.Errorf("arch attribution = %q, want the git connector", got["arch:overview"])
	}
}

func TestCrossUnitPageCollisionFailsTheLaterUnit(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	// Both module units write the exact same page: before M6 the second
	// silently overwrote the first at import.
	page := validPage("entity", "Shared")
	runner.filesBySession = map[string]map[int]map[string]string{
		"module_alpha": {0: {"entities/shared.md": page}},
		"module_beta":  {0: {"entities/shared.md": page}},
		"arch":         {0: {"synthesis/overview-notes.md": validPage("synthesis", "Notes")}},
	}
	p := testPipeline(store, runner)

	m := testMap(
		mapper.Unit{Key: "module:alpha", Slug: "alpha", Hash: "h1"},
		mapper.Unit{Key: "module:beta", Slug: "beta", Hash: "h2"},
	)
	m.Hash = "map-1"
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.Router.ModuleDirs["apps/alpha"] = diff.ModuleKey("alpha")
	req.Router.ModuleDirs["apps/beta"] = diff.ModuleKey("beta")

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Status != StatusPartial {
		t.Fatalf("status = %q, want partial (one unit failed the collision check)", res.Summary.Status)
	}

	var collided bool
	for _, v := range res.Violations {
		if strings.Contains(v.Reason, "two units must not claim one page") {
			collided = true
		}
	}
	if !collided {
		t.Errorf("collision violation missing: %+v", res.Violations)
	}
	// Exactly one copy of the page landed.
	imp := store.lastImport()
	count := 0
	for _, pg := range imp.UpsertPages {
		if pg.Path == "entities/shared.md" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared page imported %d times, want 1", count)
	}
}
