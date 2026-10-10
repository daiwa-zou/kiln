package jobs

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func TestSourceTalliesDoNotDependOnWhichConnectorSynced(t *testing.T) {
	upload := diff.DocKey(diff.UploadOrigin("notes.md"))
	web := diff.DocKey(diff.WebOrigin("https://example.com/a"))
	recorded := []diff.SourceRecord{
		{Key: diff.ModuleKey("old")},
		{Key: upload},
		{Key: diff.Key(string(upload) + "#intro")},
		{Key: web},
		{Key: diff.ArchOverview},
	}

	// What a run pinned to the git connector maps: the repository alone.
	repoOnly := &mapper.WorkspaceMap{Units: []mapper.Unit{
		{Key: "module:a"}, {Key: "module:b"}, {Key: "doc:README.md"},
	}}
	gitNS := []string{"module", "entry", "arch", "doc"}

	// What an unpinned run maps: the repository merged with the other sources.
	merged := &mapper.WorkspaceMap{Units: append(slices.Clone(repoOnly.Units),
		mapper.Unit{Key: string(upload)},
		mapper.Unit{Key: string(upload) + "#intro"},
		mapper.Unit{Key: string(web)},
	)}
	allNS := []string{"module", "entry", "arch", "doc", "doc:upload", "doc:web"}

	want := []wiki.SourceTally{
		{Kind: "module", Count: 2}, {Kind: "doc", Count: 1},
		{Kind: "doc:upload", Count: 2}, {Kind: "doc:web", Count: 1},
	}
	if got := sourceTallies(merged, recorded, nil, allNS); !reflect.DeepEqual(got, want) {
		t.Errorf("unpinned run: got %v, want %v", got, want)
	}
	// The pinned run counts the repository from its map and the rest from the
	// records, so it agrees. The record for the module the repository no
	// longer has is not counted: its namespace was synced and the map decides.
	if got := sourceTallies(repoOnly, recorded, nil, gitNS); !reflect.DeepEqual(got, want) {
		t.Errorf("pinned run: got %v, want %v", got, want)
	}

	// A source the run's cascade is deleting is gone from the tally too.
	dropped := []diff.Key{web}
	gotDropped := sourceTallies(repoOnly, recorded, dropped, gitNS)
	for _, tl := range gotDropped {
		if tl.Kind == "doc:web" {
			t.Errorf("a dropped source is still counted: %v", gotDropped)
		}
	}

	// A synced namespace that came back empty is a real zero, not a reason to
	// fall back to what used to be on file.
	noUploads := []string{"module", "entry", "arch", "doc", "doc:upload"}
	for _, tl := range sourceTallies(repoOnly, recorded, nil, noUploads) {
		if tl.Kind == "doc:upload" {
			t.Errorf("a synced-but-empty namespace is still counted: %v", tl)
		}
	}

	// Unspecified namespaces infer the synced set from the map, as deletion
	// detection does.
	if got := sourceTallies(repoOnly, recorded, nil, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("inferred namespaces: got %v, want %v", got, want)
	}
	if got := sourceTallies(nil, nil, nil, nil); len(got) != 0 {
		t.Errorf("nothing in, tallies out: %v", got)
	}
}

// TestPinnedRunKeepsTheOverviewsDocumentTally is the whole path: a bench with
// an uploaded document, then a run that syncs only the repository. The
// overview that run writes must still say the bench has a document.
func TestPinnedRunKeepsTheOverviewsDocumentTally(t *testing.T) {
	repo := archGateRepo(t)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nA document the bench was compiled from.\n"})

	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{CostPerCall: 0.01}))

	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r1", WorkspaceID: "ws-1", Trigger: "manual",
		Source: SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"},
	}); err != nil {
		t.Fatalf("unpinned build: %v", err)
	}
	if !strings.Contains(store.overview, "uploaded document") {
		t.Fatalf("overview after an unpinned build does not count the document:\n%s", store.overview)
	}

	// A structural change, built from the repository alone, the way a webhook
	// run pinned to the git connector would.
	writeTree(t, repo, map[string]string{
		"gamma/go.mod":  "module example.com/gamma\n\ngo 1.25\n",
		"gamma/main.go": "package main\n\nfunc main() {}\n",
	})
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r2", WorkspaceID: "ws-1", Trigger: "webhook",
		Source: SourceSpec{Path: repo, Slug: "bench"},
	}); err != nil {
		t.Fatalf("pinned build: %v", err)
	}
	if !strings.Contains(store.overview, "uploaded document") {
		t.Errorf("a repository-only run dropped the document from the overview's tallies:\n%s", store.overview)
	}
}
