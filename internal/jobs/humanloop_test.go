package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// page builds a minimal valid page for seeding the memStore.
func page(path, slug, pageType, title string) wiki.Page {
	return wiki.Page{
		Path: path, Slug: slug,
		Meta: wiki.Frontmatter{
			Type: wiki.PageType(pageType), Title: title,
			Created: "2026-07-25", Updated: "2026-07-25",
		},
		Body: "# " + title + "\n\nSeed body.\n",
	}
}

// reviewingRunner returns a valid page plus review flags, the way a real
// generate step raises questions alongside content.
type reviewingRunner struct {
	structuredRunner
	analysisReviews []agent.ReviewFlag
	generateReviews []agent.ReviewFlag
}

func (r *reviewingRunner) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	res, err := r.structuredRunner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.Step == agent.StepAnalyze {
		res.Analysis = &agent.AnalysisResult{Reviews: r.analysisReviews}
	} else if res.Generation != nil {
		res.Generation.Reviews = r.generateReviews
	}
	return res, nil
}

func TestReviewFlagsReachTheRunSummary(t *testing.T) {
	store := newMemStore()
	runner := &reviewingRunner{
		structuredRunner: structuredRunner{byUnit: map[string][]agent.GeneratedPage{
			"module:ripple": {{
				Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
				Body: "# Ripple\n\nCoordinates work across the services, dispatching tasks to " +
					"workers and collecting their results for downstream consumers.\n",
			}},
		}},
		analysisReviews: []agent.ReviewFlag{{Kind: "gap", Title: "No docs for the retry path", Detail: "analyze noticed"}},
		generateReviews: []agent.ReviewFlag{{Kind: "contradiction", Title: "Two retry owners", Detail: "generate noticed"}},
	}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Both steps' flags must survive into the summary the store persists --
	// dropping them was exactly the bug this feature fixes. The runner attaches
	// the same flags to every unit, and a full rebuild also runs arch:overview,
	// so assert per unit rather than in total.
	var kinds []string
	for _, rv := range res.Summary.Reviews {
		if rv.Unit == diff.Key("module:ripple") {
			kinds = append(kinds, rv.Kind)
		}
	}
	if len(kinds) != 2 || kinds[0] != "gap" || kinds[1] != "contradiction" {
		t.Fatalf("module:ripple reviews = %v, want [gap contradiction] (analyze then generate)", kinds)
	}
	if len(store.runs) == 0 || len(store.runs[0].Reviews) != len(res.Summary.Reviews) {
		t.Error("RecordRun received a different review set than the summary carries")
	}
}

func TestDisappearedSourceFilesAReviewInsteadOfDeleting(t *testing.T) {
	store := newMemStore()
	// A source on record whose key the current map no longer contains.
	store.sources[diff.Key("module:legacy")] = diff.SourceRecord{
		Key: diff.Key("module:legacy"), InputHash: "old",
		FilesWritten: []string{"entities/legacy.md"},
	}
	store.pages["entities/legacy.md"] = page("entities/legacy.md", "legacy", "entity", "Legacy")

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
			Body: "# Ripple\n\nCoordinates work across the services, dispatching tasks to " +
				"workers and collecting their results for downstream consumers.\n",
		}},
	}}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	if _, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true})); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The page must still exist -- deletion is a question, not a side effect.
	if _, ok := store.pages["entities/legacy.md"]; !ok {
		t.Fatal("legacy page deleted without approval")
	}
	if len(store.deletionReviews) != 1 || store.deletionReviews[0].Key != diff.Key("module:legacy") {
		t.Fatalf("deletion reviews = %+v, want one for module:legacy", store.deletionReviews)
	}
	if !strings.Contains(store.deletionReviews[0].Detail, "entities/legacy.md") {
		t.Errorf("review detail %q does not name the page at stake", store.deletionReviews[0].Detail)
	}
}

func TestApprovedDeletionExecutesTheCascade(t *testing.T) {
	store := newMemStore()
	store.sources[diff.Key("module:legacy")] = diff.SourceRecord{
		Key: diff.Key("module:legacy"), InputHash: "old",
		FilesWritten: []string{"entities/legacy.md"},
	}
	store.pages["entities/legacy.md"] = page("entities/legacy.md", "legacy", "entity", "Legacy")
	// The approval recorded through the review queue.
	store.approvedDeletions = []diff.Key{diff.Key("module:legacy")}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{}}
	p := testPipeline(store, runner)
	m := testMap() // the legacy module is gone from the map entirely

	res, err := p.Build(context.Background(), testRequest(t, m, diff.ChangeSet{FullRebuild: true}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := store.pages["entities/legacy.md"]; ok {
		t.Error("approved deletion did not remove the page")
	}
	if _, ok := store.sources[diff.Key("module:legacy")]; ok {
		t.Error("approved deletion did not drop the source record")
	}
	if res.Summary.Deleted != 1 {
		t.Errorf("summary.Deleted = %d, want 1", res.Summary.Deleted)
	}
	// Approved and gone: no new review should be raised for it.
	if len(store.deletionReviews) != 0 {
		t.Errorf("deletion reviews after approval = %+v, want none", store.deletionReviews)
	}
}

func TestDryRunFilesNoDeletionReviews(t *testing.T) {
	store := newMemStore()
	store.sources[diff.Key("module:legacy")] = diff.SourceRecord{
		Key: diff.Key("module:legacy"), InputHash: "old",
	}

	p := testPipeline(store, &structuredRunner{})
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})

	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.DryRun = true
	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	// A dry run estimates; it must not write review items.
	if len(store.deletionReviews) != 0 {
		t.Errorf("dry run filed deletion reviews: %+v", store.deletionReviews)
	}
}
