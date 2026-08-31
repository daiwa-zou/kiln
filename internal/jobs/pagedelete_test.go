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

// TestBuildDiscardsAPageAHumanDeleted is the guarantee the whole feature rests
// on: the source is unchanged and would happily produce this page again, and it
// must not come back.
func TestBuildDiscardsAPageAHumanDeleted(t *testing.T) {
	store := newMemStore()
	store.steering = Steering{
		Suppressed: map[string]string{"ripple": "duplicate of the dispatcher page"},
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		"module:ripple": {
			{
				Path: "entities/ripple.md", Type: "entity", Title: "Ripple",
				Body: "# Ripple\n\n" + strings.Repeat("The page a human deleted. ", 8),
			},
			{
				Path: "concepts/dispatch.md", Type: "concept", Title: "Dispatch",
				Body: "# Dispatch\n\n" + strings.Repeat("A page nobody deleted. ", 8),
			},
		},
	}}

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("Status = %q, violations = %v", res.Summary.Status, res.Violations)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing imported")
	}
	for _, pg := range imp.UpsertPages {
		if pg.Slug == "ripple" {
			t.Error("a deleted page was written back by the next build")
		}
	}
	// The rest of the unit's work still lands: a suppression removes one page,
	// not the unit that happened to produce it.
	var kept bool
	for _, pg := range imp.UpsertPages {
		if pg.Slug == "dispatch" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the unit's other page was lost: %+v", imp.UpsertPages)
	}
}

// TestGeneratePromptNamesDeletedPages: the agent is told, not merely overruled.
// Being told is what stops a run paying to write a page that gets discarded.
func TestGeneratePromptNamesDeletedPages(t *testing.T) {
	s := Steering{Suppressed: map[string]string{
		"ripple":   "duplicate of the dispatcher page",
		"unwanted": "",
	}}

	prompt := generatePrompt("module:ripple", mapper.Unit{Slug: "ripple"},
		"", "", s, "", nil, 0, nil)

	if !strings.Contains(prompt, "Pages a human has deleted") {
		t.Fatalf("generate prompt does not mention deleted pages:\n%s", prompt)
	}
	if !strings.Contains(prompt, "duplicate of the dispatcher page") {
		t.Error("the human's reason was not passed on")
	}
	if !strings.Contains(prompt, "`unwanted`") {
		t.Error("a deletion with no reason was omitted entirely")
	}

	// Analysis too: a page that must not be written should not be planned.
	plan := analyzePrompt("module:ripple", mapper.Unit{Slug: "ripple"}, "", s, 0, nil)
	if !strings.Contains(plan, "Pages a human has deleted") {
		t.Error("analyze prompt does not mention deleted pages")
	}
}

// pagesWithSlugs builds the minimal pages dropSuppressed looks at.
func pagesWithSlugs(slugs ...string) []wiki.Page {
	out := make([]wiki.Page, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, wiki.Page{Slug: s, Path: "concepts/" + s + ".md"})
	}
	return out
}

func slugsOf(pages []wiki.Page) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.Slug)
	}
	return out
}

func TestDropSuppressedLeavesOtherPagesAlone(t *testing.T) {
	p := testPipeline(newMemStore(), &structuredRunner{})

	got := dropSuppressed(
		pagesWithSlugs("a", "b", "c"),
		map[string]string{"b": "not wanted"},
		p.logger(),
	)

	if len(got) != 2 || got[0].Slug != "a" || got[1].Slug != "c" {
		t.Errorf("kept %v, want a and c", slugsOf(got))
	}
}

func TestDropSuppressedWithNothingDeletedIsAPassThrough(t *testing.T) {
	p := testPipeline(newMemStore(), &structuredRunner{})

	if got := dropSuppressed(pagesWithSlugs("a", "b"), nil, p.logger()); len(got) != 2 {
		t.Errorf("kept %v, want both", slugsOf(got))
	}
}
