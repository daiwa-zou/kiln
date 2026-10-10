package jobs

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
)

// These tests pin what gates the architecture synthesis. It reads the
// repository's module structure, so only that may decide whether it
// regenerates: not which connectors a run happened to sync, and not what the
// documents say.

func archGateRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"alpha/go.mod":  "module example.com/alpha\n\ngo 1.25\n",
		"alpha/main.go": "package main\n\nfunc main() {}\n",
		"beta/go.mod":   "module example.com/beta\n\ngo 1.25\n",
		"beta/lib.go":   "package beta\n\nfunc Beta() int { return 1 }\n",
	})
	return repo
}

// archHashOf maps a source the way a run does and returns the gate's input.
func archHashOf(t *testing.T, src SourceSpec) string {
	t.Helper()
	mat, err := materialize(context.Background(), io.Discard, src)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer mat.Close()
	return mat.Map.ArchHash
}

func TestArchGateHashIgnoresDocuments(t *testing.T) {
	repo := archGateRepo(t)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nFirst draft of the notes.\n"})

	// A run pinned to the git connector maps the repository alone; an unpinned
	// one maps the repository merged with the documents. Same repository, so
	// the same gate -- otherwise alternating webhook and manual runs regenerate
	// the synthesis every time with identical inputs.
	pinned := archHashOf(t, SourceSpec{Path: repo, Slug: "bench"})
	if pinned == "" {
		t.Fatal("a repository map has no arch hash; the synthesis could never be skipped")
	}
	unpinned := archHashOf(t, SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"})
	if unpinned != pinned {
		t.Errorf("pinned and unpinned runs over one repository gate differently:\n  %s\n  %s", pinned, unpinned)
	}

	// Editing a document, or adding another, is not a reason to rewrite the
	// architecture.
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nA thoroughly rewritten set of notes.\n"})
	if got := archHashOf(t, SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"}); got != pinned {
		t.Errorf("editing a document moved the arch hash")
	}
	writeTree(t, docs, map[string]string{"more.md": "# More\n\nAnother document entirely.\n"})
	if got := archHashOf(t, SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"}); got != pinned {
		t.Errorf("adding a document moved the arch hash")
	}
}

func TestArchGateHashTracksModuleStructure(t *testing.T) {
	repo := archGateRepo(t)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nBody.\n"})

	before := archHashOf(t, SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"})

	writeTree(t, repo, map[string]string{
		"gamma/go.mod":  "module example.com/gamma\n\ngo 1.25\n",
		"gamma/main.go": "package main\n\nfunc main() {}\n",
	})
	after := archHashOf(t, SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"})
	if after == before {
		t.Error("a new module did not move the arch hash")
	}
}

// archPlanned dry-runs a build against the store and reports whether the
// synthesis and the named document would regenerate.
func archPlanned(t *testing.T, p *Pipeline, src SourceSpec, runID string) (arch bool, planned []diff.Key) {
	t.Helper()
	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: runID, WorkspaceID: "ws-1", Trigger: "manual",
		Source: src, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Execute %s: %v", runID, err)
	}
	return slices.Contains(res.Planned, diff.ArchOverview), res.Planned
}

func TestArchOverviewRegeneratesOnlyOnModuleStructure(t *testing.T) {
	repo := archGateRepo(t)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nFirst draft of the notes.\n"})
	notes := diff.DocKey(diff.UploadOrigin("notes.md"))

	store := newMemStore()
	p := testPipeline(store, agent.NewFakeRunner(agent.FakeOptions{CostPerCall: 0.01}))
	repoOnly := SourceSpec{Path: repo, Slug: "bench"}
	both := SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"}

	// The first, repository-only build pays for the synthesis once.
	if arch, _ := archPlanned(t, p, repoOnly, "r0"); !arch {
		t.Fatal("a first build does not plan the architecture synthesis")
	}
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r1", WorkspaceID: "ws-1", Trigger: "manual", Source: repoOnly,
	}); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if arch, planned := archPlanned(t, p, repoOnly, "r1b"); arch || len(planned) != 0 {
		t.Fatalf("an unchanged repeat build plans work: %v", planned)
	}

	// Connecting documents plans the document, not the architecture. This used
	// to replace the gate with a content hash over every unit.
	arch, planned := archPlanned(t, p, both, "r2")
	if arch {
		t.Errorf("adding documents re-plans the architecture synthesis: %v", planned)
	}
	if !slices.Contains(planned, notes) {
		t.Errorf("the new document is not planned: %v", planned)
	}
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r2b", WorkspaceID: "ws-1", Trigger: "manual", Source: both,
	}); err != nil {
		t.Fatalf("build with documents: %v", err)
	}

	// Editing the document regenerates the document alone.
	writeTree(t, docs, map[string]string{"notes.md": "# Notes\n\nRewritten from top to bottom.\n"})
	arch, planned = archPlanned(t, p, both, "r3")
	if arch {
		t.Errorf("editing a document re-plans the architecture synthesis: %v", planned)
	}
	if !slices.Contains(planned, notes) {
		t.Errorf("the edited document is not planned: %v", planned)
	}

	// A run pinned to the repository, then one that is not, over the same
	// repository: neither re-plans the synthesis.
	if arch, planned := archPlanned(t, p, repoOnly, "r4"); arch {
		t.Errorf("a repository-only run after an unpinned one re-plans the synthesis: %v", planned)
	}

	// A structural change is what does.
	writeTree(t, repo, map[string]string{
		"gamma/go.mod":  "module example.com/gamma\n\ngo 1.25\n",
		"gamma/main.go": "package main\n\nfunc main() {}\n",
	})
	if arch, planned := archPlanned(t, p, both, "r5"); !arch {
		t.Errorf("a new module does not re-plan the synthesis: %v", planned)
	}
}
