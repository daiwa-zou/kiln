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

func TestMergeHashIgnoresConnectorOrder(t *testing.T) {
	// The hash gates whether a run costs anything. If it depended on which
	// connector synced first, a reordering would look like a full rebuild.
	code := &WorkspaceMap{Kind: "repo", Units: []Unit{unit("module:auth", "h1")}}
	docs := &WorkspaceMap{Kind: "doc", Units: []Unit{unit("doc:a.pdf", "h2")}}

	a, err := Merge("/repo", code, docs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Merge("/repo", docs, code)
	if err != nil {
		t.Fatal(err)
	}

	if a.Hash != b.Hash {
		t.Errorf("hash depends on merge order: %s vs %s", a.Hash, b.Hash)
	}
}

func TestMergeHashTracksContent(t *testing.T) {
	base := &WorkspaceMap{Kind: "repo", Units: []Unit{unit("module:auth", "h1")}}
	changed := &WorkspaceMap{Kind: "repo", Units: []Unit{unit("module:auth", "h2")}}

	a, _ := Merge("/repo", base)
	b, _ := Merge("/repo", changed)
	if a.Hash == b.Hash {
		t.Error("a changed unit hash did not change the map hash")
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
