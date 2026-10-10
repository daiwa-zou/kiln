package mapper

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func unit(key, hash string) Unit { return Unit{Key: key, Hash: hash} }

func TestMergeCombinesUnitsAndEdges(t *testing.T) {
	code := &WorkspaceMap{
		Kind:  "repo",
		Units: []Unit{unit("module:auth", "h1"), unit("module:api", "h2")},
		Edges: []Edge{{From: "module:api", To: "module:auth", Kind: "requires"}},
	}
	docs := &WorkspaceMap{
		Kind:  "doc",
		Units: []Unit{unit("doc:handbook.pdf", "h3")},
	}

	got, err := Merge("/repo", code, docs)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Units) != 3 {
		t.Errorf("got %d units, want 3", len(got.Units))
	}
	if len(got.Edges) != 1 {
		t.Errorf("got %d edges, want 1", len(got.Edges))
	}
	// A workspace holding both kinds is the claim the connector abstraction
	// makes; labelling it by one of them would misreport it.
	if got.Kind != MergeKind {
		t.Errorf("kind = %q, want %q", got.Kind, MergeKind)
	}
	if got.Root != "/repo" {
		t.Errorf("root = %q", got.Root)
	}
}

func TestMergeKeepsASingleKindUnlabelled(t *testing.T) {
	got, err := Merge("/repo", &WorkspaceMap{Kind: "repo", Units: []Unit{unit("a", "h")}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "repo" {
		t.Errorf("kind = %q, want the single contributing kind", got.Kind)
	}
}

func TestMergeRejectsDuplicateKeys(t *testing.T) {
	// Two producers for one page means one is discarded on every run, and it
	// surfaces as a page that mysteriously never updates. Fail loudly instead.
	_, err := Merge("/repo",
		&WorkspaceMap{Kind: "repo", Units: []Unit{unit("doc:README.md", "h1")}},
		&WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:README.md", "h2")}},
	)

	var dup *DuplicateKeyError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want a DuplicateKeyError", err)
	}
	if dup.Key != "doc:README.md" {
		t.Errorf("key = %q", dup.Key)
	}
	// The message has to name both mappers, or finding the collision means
	// reading every mapper in the tree.
	if msg := dup.Error(); !strings.Contains(msg, "repo") || !strings.Contains(msg, "doc") {
		t.Errorf("error does not name both mappers: %s", msg)
	}
}

// repoMap is a repository-shaped input carrying the architecture digest the
// repository mapper would have computed.
func repoMap(archHash string, units ...Unit) *WorkspaceMap {
	return &WorkspaceMap{Kind: "repo", ArchHash: archHash, Units: units}
}

func TestMergeCarriesArchHashThroughUnchanged(t *testing.T) {
	// The architecture synthesis reads the repository's module structure. A
	// repository merged with documents must gate it exactly as the repository
	// alone does, or every document edit re-pays for the synthesis and a run
	// pinned to the git connector disagrees with one that is not.
	code := repoMap("structure-1", unit("module:auth", "h1"))
	docs := &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "h2")}}

	alone, err := Merge("/repo", code)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := Merge("/repo", code, docs)
	if err != nil {
		t.Fatal(err)
	}
	if alone.ArchHash != "structure-1" || merged.ArchHash != alone.ArchHash {
		t.Errorf("arch hash = %q merged, %q alone; want both structure-1", merged.ArchHash, alone.ArchHash)
	}
}

func TestMergeArchHashIgnoresDocumentContent(t *testing.T) {
	code := repoMap("structure-1", unit("module:auth", "h1"))

	a, _ := Merge("/repo", code, &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "draft")}})
	b, _ := Merge("/repo", code, &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "final")}})
	c, _ := Merge("/repo", code, &WorkspaceMap{Kind: "doc", Units: []Unit{
		unit("doc:a.pdf", "final"), unit("doc:b.pdf", "new"),
	}})
	if a.ArchHash != b.ArchHash || a.ArchHash != c.ArchHash {
		t.Errorf("a document edit or addition moved the arch hash: %q %q %q", a.ArchHash, b.ArchHash, c.ArchHash)
	}
}

func TestMergeArchHashTracksTheRepository(t *testing.T) {
	docs := &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "h2")}}

	a, _ := Merge("/repo", repoMap("structure-1", unit("module:auth", "h1")), docs)
	b, _ := Merge("/repo", repoMap("structure-2", unit("module:auth", "h1")), docs)
	if a.ArchHash == b.ArchHash {
		t.Error("a structural change did not change the arch hash")
	}
}

func TestMergeArchHashIgnoresConnectorOrder(t *testing.T) {
	// The gate decides whether a run costs anything. If it depended on which
	// connector synced first, a reordering would look like a full rebuild.
	code := repoMap("structure-1", unit("module:auth", "h1"))
	other := repoMap("structure-2", unit("module:web", "h3"))
	docs := &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "h2")}}

	a, err := Merge("/repo", code, docs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Merge("/repo", docs, code)
	if err != nil {
		t.Fatal(err)
	}
	if a.ArchHash != b.ArchHash {
		t.Errorf("arch hash depends on merge order: %s vs %s", a.ArchHash, b.ArchHash)
	}

	// Two repositories combine, and do so the same way in either order.
	ab, _ := Merge("/repo", code, other)
	ba, _ := Merge("/repo", other, code)
	if ab.ArchHash == "" || ab.ArchHash != ba.ArchHash {
		t.Errorf("combined arch hash depends on merge order: %q vs %q", ab.ArchHash, ba.ArchHash)
	}
	if ab.ArchHash == code.ArchHash || ab.ArchHash == other.ArchHash {
		t.Error("combined arch hash ignores one of its repositories")
	}
}

func TestMergeWithoutRepositoryHasNoArchHash(t *testing.T) {
	// Documents alone have no module structure for a synthesis to describe.
	got, err := Merge("/docs", &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "h2")}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchHash != "" {
		t.Errorf("arch hash = %q, want none", got.ArchHash)
	}
}

func TestMergeReportsTheFreshestGeneration(t *testing.T) {
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	got, err := Merge("/repo",
		&WorkspaceMap{Kind: "repo", GeneratedAt: older},
		&WorkspaceMap{Kind: "doc", GeneratedAt: newer},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GeneratedAt.Equal(newer) {
		t.Errorf("generatedAt = %v, want %v", got.GeneratedAt, newer)
	}
}

func TestMergeSkipsNilMaps(t *testing.T) {
	// A workspace with no document connector passes nil rather than branching
	// at every call site.
	got, err := Merge("/repo", &WorkspaceMap{Kind: "repo", Units: []Unit{unit("a", "h")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Units) != 1 {
		t.Errorf("got %d units, want 1", len(got.Units))
	}
}
