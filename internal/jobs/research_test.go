package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// researchRepo is a small bench whose two modules are about different things,
// so a question about one must not select the other.
func researchRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.25\n",
		"internal/dispatch/dispatch.go": "package dispatch\n\n" +
			"// Dispatch hands work to the workers and collects their results.\n" +
			"func Dispatch() {}\n",
		"internal/billing/billing.go": "package billing\n\n" +
			"// Billing totals the ledger at the end of each window.\n" +
			"func Total() {}\n",
	})
	return repo
}

func TestResearchAnswersAQuestionAndLedgersTheRun(t *testing.T) {
	repo := researchRepo(t)
	store := newMemStore()
	runner := newScriptedRunner()
	runner.researchResult = &agent.ResearchResult{
		Findings: "Dispatch retries three times, per internal/dispatch/dispatch.go.",
		Evidence: []string{"internal/dispatch/dispatch.go"},
	}
	p := testPipeline(store, runner)

	out, err := p.Research(context.Background(), ResearchRequest{
		RunID:       "run-research",
		WorkspaceID: "ws-1",
		Source:      SourceSpec{Path: repo, Slug: "bench"},
		Question: Question{
			Kind:   "uncertain",
			Title:  "How many times does dispatch retry?",
			Detail: "The dispatch module's retry count could not be corroborated.",
		},
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if !strings.Contains(out.Findings, "retries three times") {
		t.Errorf("findings = %q, want the scripted answer", out.Findings)
	}
	if out.Resolved {
		t.Error("an answer that did not claim resolution must not report one")
	}
	if out.CostUSD != runner.costPerCall {
		t.Errorf("cost = %v, want the call's %v", out.CostUSD, runner.costPerCall)
	}

	// Exactly one call, and it is a research call: research is a single read,
	// not a generation pass, and paying for an analyze it never uses would be
	// the easiest way for this to become quietly expensive.
	if n := len(runner.calls); n != 1 {
		t.Fatalf("made %d agent calls, want 1", n)
	}
	if runner.calls[0].Step != agent.StepResearch {
		t.Errorf("step = %q, want %q", runner.calls[0].Step, agent.StepResearch)
	}
	if runner.calls[0].JSONSchema == "" {
		t.Error("research must be schema-constrained like analyze")
	}

	// The run has to reach the ledger, or the spend window undercounts every
	// question anyone ever asks.
	if len(store.runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(store.runs))
	}
	rec := store.runs[0]
	if rec.Trigger != "research" || rec.Status != StatusSucceeded {
		t.Errorf("run recorded as %s/%s, want research/succeeded", rec.Trigger, rec.Status)
	}
	if rec.CostUSD != runner.costPerCall {
		t.Errorf("ledgered cost = %v, want %v", rec.CostUSD, runner.costPerCall)
	}
	if rec.Created != 0 || rec.Updated != 0 || rec.Deleted != 0 {
		t.Errorf("research touched pages: %+v", rec)
	}
	if len(store.imports) != 0 {
		t.Errorf("research imported %d times; it must write nothing", len(store.imports))
	}
}

func TestResearchReadsEnvelopeTextWhenTheRunnerReturnsNoStructure(t *testing.T) {
	repo := researchRepo(t)
	store := newMemStore()
	runner := newScriptedRunner()
	// The CLI runner path: schema-constrained JSON arrives as envelope text.
	runner.researchText = `{"findings":"Both sources agree.","resolved":true}`
	p := testPipeline(store, runner)

	out, err := p.Research(context.Background(), ResearchRequest{
		RunID: "run-research", WorkspaceID: "ws-1",
		Source:   SourceSpec{Path: repo, Slug: "bench"},
		Question: Question{Kind: "contradiction", Title: "Do the two ledgers agree?"},
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if out.Findings != "Both sources agree." || !out.Resolved {
		t.Errorf("parsed %+v, want the envelope's findings and resolution", out)
	}
}

func TestResearchLedgersAFailedPass(t *testing.T) {
	repo := researchRepo(t)
	store := newMemStore()
	runner := newScriptedRunner()
	runner.failWith = errors.New("model unavailable")
	runner.failWithCost = true
	p := testPipeline(store, runner)

	_, err := p.Research(context.Background(), ResearchRequest{
		RunID: "run-research", WorkspaceID: "ws-1",
		Source:   SourceSpec{Path: repo, Slug: "bench"},
		Question: Question{Kind: "gap", Title: "What is the retry policy?"},
	})
	if err == nil {
		t.Fatal("Research succeeded despite a failing runner")
	}

	// The call was billed even though it failed, so the run must carry the
	// cost: a spend window that ignores failed research undercounts forever.
	if len(store.runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(store.runs))
	}
	if store.runs[0].Status != StatusFailed {
		t.Errorf("status = %s, want failed", store.runs[0].Status)
	}
	if store.runs[0].CostUSD != runner.costPerCall {
		t.Errorf("failed pass ledgered %v, want the %v it really spent",
			store.runs[0].CostUSD, runner.costPerCall)
	}
}

func TestResearchRejectsAnEmptyAnswer(t *testing.T) {
	repo := researchRepo(t)
	store := newMemStore()
	runner := newScriptedRunner()
	runner.researchResult = &agent.ResearchResult{Findings: "   "}
	p := testPipeline(store, runner)

	if _, err := p.Research(context.Background(), ResearchRequest{
		RunID: "run-research", WorkspaceID: "ws-1",
		Source:   SourceSpec{Path: repo, Slug: "bench"},
		Question: Question{Kind: "gap", Title: "Anything?"},
	}); err == nil {
		t.Fatal("blank findings accepted; a card would show an empty answer")
	}
}

func TestResearchNeedsAQuestion(t *testing.T) {
	p := testPipeline(newMemStore(), newScriptedRunner())
	if _, err := p.Research(context.Background(), ResearchRequest{
		RunID: "run-research", WorkspaceID: "ws-1",
		Source: SourceSpec{Path: t.TempDir()},
	}); err == nil {
		t.Fatal("research ran without a question")
	}
}

func TestRelevantUnitsRanksBySubjectAndCaps(t *testing.T) {
	wm := testMap(
		mapper.Unit{Key: "module:dispatch", Title: "Dispatch", Dir: "internal/dispatch"},
		mapper.Unit{Key: "module:billing", Title: "Billing", Dir: "internal/billing"},
		mapper.Unit{Key: "module:ghost", Title: "Ghost", Empty: true},
	)

	got := relevantUnits(Question{
		Title:  "How many times does dispatch retry?",
		Detail: "The retry count in the dispatch module could not be corroborated.",
	}, wm)

	if len(got) == 0 {
		t.Fatal("a question naming a module selected nothing")
	}
	if got[0].Key != "module:dispatch" {
		t.Errorf("top unit = %s, want module:dispatch", got[0].Key)
	}
	for _, u := range got {
		if u.Key == "module:ghost" {
			t.Error("an empty unit was selected; there is nothing in it to read")
		}
	}

	// A question with nothing to match on selects nothing rather than
	// everything: an unfocused question would otherwise render the whole
	// bench into one prompt.
	if n := len(relevantUnits(Question{Title: "the and but"}, wm)); n != 0 {
		t.Errorf("stopwords-only question selected %d units, want 0", n)
	}
}

func TestRelevantUnitsCapsTheSelection(t *testing.T) {
	var units []mapper.Unit
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		units = append(units, mapper.Unit{
			Key: "module:dispatch-" + name, Title: "Dispatch " + name,
		})
	}
	got := relevantUnits(Question{Title: "dispatch behaviour"}, testMap(units...))
	if len(got) != maxResearchUnits {
		t.Errorf("selected %d units, want the cap of %d", len(got), maxResearchUnits)
	}
}

func TestRelevantPagesPrefersTheSubjectOfAGap(t *testing.T) {
	pages := []wiki.Page{
		{Slug: "retry-policy", Meta: wiki.Frontmatter{Title: "Retry policy"}, Body: "A page about retries."},
		{Slug: "dispatch", Meta: wiki.Frontmatter{Title: "Dispatch"},
			Body: "Dispatch defers to [[retry-policy]] for how often it tries again."},
		{Slug: "unrelated", Meta: wiki.Frontmatter{Title: "Onboarding"}, Body: "Nothing to do with it."},
	}

	got := relevantPages(Question{
		Kind: "gap", Title: "retry-policy is referred to but not written",
		PageSlug: "retry-policy",
	}, pages)

	if len(got) == 0 {
		t.Fatal("a gap selected no pages; the pages naming it are the statement of what is missing")
	}
	// The page linking to the gap must be in the selection: it is what declared
	// the gap, and it is where the answer has to fit.
	var sawLinker bool
	for _, p := range got {
		if p.Slug == "dispatch" {
			sawLinker = true
		}
		if p.Slug == "unrelated" {
			t.Error("an unrelated page was selected")
		}
	}
	if !sawLinker {
		t.Error("the page linking to the gap was not selected")
	}
}

func TestResearchPromptCarriesTheQuestionAndItsMaterial(t *testing.T) {
	prompt := researchPrompt(
		Question{Kind: "contradiction", Title: "Two retry counts", Detail: "one says 3, one says 5", PageSlug: "dispatch"},
		"",
		[]mapper.Unit{{Key: "module:dispatch", Title: "Dispatch"}},
		[]wiki.Page{{Slug: "dispatch", Meta: wiki.Frontmatter{Title: "Dispatch"}, Body: "It retries."}},
	)

	// The "Question:" line leads, which is the contract the fake runner reads
	// back and the reason a research prompt is recognisable at all.
	if !strings.Contains(prompt, "Question: Two retry counts") {
		t.Errorf("prompt does not lead with the question:\n%s", prompt)
	}
	for _, want := range []string{"contradiction", "one says 3", "module:dispatch", "dispatch", "It retries."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestResearchSystemPromptRefusesToWriteAndKeepsTheUntrustedFraming(t *testing.T) {
	sys := researchSystemPrompt(Steering{Purpose: "Document on-call."})
	if !strings.Contains(sys, untrustedFraming) {
		t.Error("research dropped the untrusted-data framing every other step carries")
	}
	if !strings.Contains(sys, "Do not write") {
		t.Error("research must be told it writes nothing")
	}
	if !strings.Contains(sys, "Document on-call.") {
		t.Error("steering is not reaching the research prompt")
	}
}
