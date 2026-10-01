package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// A page has two names and only one identity. The path is where it sits; the
// slug is what the database keys it by -- pages upsert ON CONFLICT (wiki, slug),
// so a page rewritten under a new directory would replace the old row and leave
// the old path listed in an index built from both.
//
// The pipeline never has to reconcile that, because validation refuses the move
// outright: an existing slug may only be written to the path it already has.
// That refusal is load-bearing -- mergePages and countChanges both decide "same
// page?" by path, which is correct only while this holds -- and it had no test.

func TestBuildRefusesToMoveAnExistingPageBetweenTypes(t *testing.T) {
	store := newMemStore()
	store.pages["concepts/ripple.md"] = page("concepts/ripple.md", "ripple", "concept", "Ripple")

	// Same slug, reclassified into another type directory.
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

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var refused bool
	for _, v := range res.Violations {
		if v.Path == "entities/ripple.md" && strings.Contains(v.Reason, "already belongs to existing page") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("the move was not refused; violations: %v", res.Violations)
	}

	// And the refusal has to be total: a page that reached the store would
	// take the old row's slug with it, which is the data loss the rule exists
	// to prevent.
	if imp := store.lastImport(); imp != nil {
		for _, pg := range imp.UpsertPages {
			if pg.Path == "entities/ripple.md" {
				t.Errorf("the refused page was imported anyway: %+v", pg.Path)
			}
		}
	}
	if _, ok := store.pages["concepts/ripple.md"]; !ok {
		t.Error("the existing page was lost even though its replacement was refused")
	}
}

// The same rule seen from the other side: writing an existing slug back to the
// path it already occupies is an ordinary update and must not trip the check.
func TestBuildAllowsRewritingAnExistingPageInPlace(t *testing.T) {
	store := newMemStore()
	store.pages["entities/ripple.md"] = page("entities/ripple.md", "ripple", "entity", "Ripple")

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

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (violations: %v)", res.Summary.Status, res.Violations)
	}
	if res.Summary.Created != 0 || res.Summary.Updated != 1 {
		t.Errorf("summary = (created %d, updated %d), want (0, 1)",
			res.Summary.Created, res.Summary.Updated)
	}
	// Once per section it belongs to -- the catalog section and the recent
	// tail -- and never under a type directory it does not live in.
	if strings.Contains(store.index, "concepts/ripple") {
		t.Errorf("the index lists the page under a type it does not have:\n%s", store.index)
	}
}
