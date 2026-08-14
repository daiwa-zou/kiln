package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// pipelineStore is the narrow slice of jobs.Store a research pass touches:
// pages and steering in, a run record out. Everything a build needs and
// research does not is present only to satisfy the interface, which is itself
// the assertion -- a research pass that started importing or loading sources
// would have to change this file to compile.
type pipelineStore struct {
	pages   []wiki.Page
	pageErr error
	runs    []jobs.RunSummary
}

func (s *pipelineStore) LoadPages(context.Context, string) ([]wiki.Page, error) {
	return s.pages, s.pageErr
}
func (s *pipelineStore) LoadSteering(context.Context, string) (jobs.Steering, error) {
	return jobs.Steering{}, nil
}
func (s *pipelineStore) RecordRun(_ context.Context, run jobs.RunSummary) error {
	s.runs = append(s.runs, run)
	return nil
}
func (s *pipelineStore) LoadSources(context.Context, string) ([]diff.SourceRecord, error) {
	return nil, nil
}
func (s *pipelineStore) Import(context.Context, jobs.ImportRequest) error { return nil }

func (s *pipelineStore) RenameDocuments(context.Context, string, map[string]string) error {
	return nil
}
func (s *pipelineStore) SeedRunItems(context.Context, string, []diff.Key, float64) error {
	return nil
}
func (s *pipelineStore) MarkRunItem(context.Context, string, jobs.ItemSummary) error { return nil }
func (s *pipelineStore) LoadApprovedDeletions(context.Context, string) ([]diff.Key, error) {
	return nil, nil
}
func (s *pipelineStore) EnsureDeletionReviews(context.Context, string, []jobs.DeletionCandidate) error {
	return nil
}
func (s *pipelineStore) TrailingUnitCost(context.Context, string) (float64, error) { return 0, nil }

// Research reads a repository, which carries no figures; these exist to
// satisfy the interface, not to be exercised.
func (s *pipelineStore) ReplaceFigures(context.Context, string, string, []jobs.FigureRecord) ([]string, error) {
	return nil, nil
}

func (s *pipelineStore) FiguresForSources(context.Context, string, []string) ([]jobs.FigureRecord, error) {
	return nil, nil
}

// researchWorker wires a worker whose bench is one small repository and whose
// agent is the free fake, so the whole claim-read-write-back path runs without
// an API key.
func researchWorker(t *testing.T, st *fakeStore, ps *pipelineStore) *Worker {
	t.Helper()

	repo := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":  "module example.com/demo\n\ngo 1.25\n",
		"main.go": "package main\n\n// Dispatch hands work to workers.\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st.connectors = []store.ConnectorRow{
		{ID: "c1", Kind: "git", Name: "code", Config: map[string]any{"path": repo}},
	}
	return &Worker{
		Store: st,
		Pipeline: &jobs.Pipeline{
			Store:  ps,
			Runner: agent.NewFakeRunner(agent.FakeOptions{CostPerCall: 0.02}),
			Budget: jobs.Budget{AnalyzeUSD: 0.4, PageUSD: 1, RunUSD: 4},
			Model:  "fake",
		},
		PermittedSourceRoots: []string{repo},
	}
}

func TestProcessAnswersAResearchRunAndWritesItBack(t *testing.T) {
	st := &fakeStore{
		question: &store.ResearchQuestion{
			ReviewID: "rv-1", Kind: "gap",
			Title:  "dispatch-retries is referred to but not written",
			Detail: "Two pages link to it.",
		},
	}
	ps := &pipelineStore{}
	w := researchWorker(t, st, ps)

	w.process(context.Background(), &store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research",
	})

	if len(st.recorded) != 1 {
		t.Fatalf("wrote back %d times, want 1: %v", len(st.recorded), st.recorded)
	}
	got := st.recorded[0]
	if !strings.HasPrefix(got, "rv-1|false|") {
		t.Errorf("write-back = %q, want findings against rv-1 that resolve nothing", got)
	}
	// Evidence is appended to the prose rather than dropped: the card is read,
	// not queried, so a citation the reader cannot see was never made.
	if !strings.Contains(got, "Evidence:") {
		t.Errorf("write-back lost its evidence: %q", got)
	}

	// The run is ledgered as research, and nothing was imported.
	if len(ps.runs) != 1 || ps.runs[0].Trigger != "research" {
		t.Fatalf("runs recorded = %+v", ps.runs)
	}
	if ps.runs[0].Status != jobs.StatusSucceeded {
		t.Errorf("status = %s, want succeeded", ps.runs[0].Status)
	}
	if ps.runs[0].CostUSD == 0 {
		t.Error("research spent nothing; the fake runner charges per call and it must be ledgered")
	}
}

func TestProcessFailsAResearchRunWhoseQuestionIsGone(t *testing.T) {
	// question is nil: the item was resolved by a human, or swept with its
	// workspace, between enqueue and claim.
	st := &fakeStore{}
	ps := &pipelineStore{}
	w := researchWorker(t, st, ps)

	w.process(context.Background(), &store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research",
	})

	if len(st.recorded) != 0 {
		t.Errorf("wrote findings for a question that no longer exists: %v", st.recorded)
	}
	if len(ps.runs) != 0 {
		t.Errorf("the pipeline ran without a question: %+v", ps.runs)
	}
}

func TestProcessWritesAResearchFailureOntoTheItem(t *testing.T) {
	st := &fakeStore{
		question: &store.ResearchQuestion{ReviewID: "rv-1", Kind: "uncertain", Title: "Which is it?"},
	}
	ps := &pipelineStore{pageErr: errors.New("database unreachable")}
	w := researchWorker(t, st, ps)

	w.process(context.Background(), &store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research",
	})

	// The item is left with an account of why there are no findings, rather
	// than silently unchanged while the reason sits in a run row nobody links
	// to from the review queue.
	if len(st.recorded) != 1 {
		t.Fatalf("wrote back %d times, want 1: %v", len(st.recorded), st.recorded)
	}
	if !strings.Contains(st.recorded[0], "database unreachable") {
		t.Errorf("write-back = %q, want the failure's cause", st.recorded[0])
	}
	if strings.Contains(st.recorded[0], "|true|") {
		t.Error("a failed pass resolved the item")
	}
}

func TestFindingsTextAppendsEvidenceOnlyWhenThereIsSome(t *testing.T) {
	plain := findingsText(&jobs.ResearchOutcome{Findings: "It retries three times."})
	if plain != "It retries three times." {
		t.Errorf("findingsText with no evidence = %q, want the findings alone", plain)
	}

	cited := findingsText(&jobs.ResearchOutcome{
		Findings: "It retries three times.",
		Evidence: []string{"internal/dispatch/dispatch.go:42", "docs/runbook.md"},
	})
	for _, want := range []string{"It retries three times.", "Evidence:", "dispatch.go:42", "runbook.md"} {
		if !strings.Contains(cited, want) {
			t.Errorf("findingsText is missing %q:\n%s", want, cited)
		}
	}
}
