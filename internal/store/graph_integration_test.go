package store

import (
	"context"
	"testing"

	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func TestGraphReturnsNodesAndResolvedEdges(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	page := func(slug, body string) wiki.Page {
		p := wiki.Page{Path: "entities/" + slug + ".md", Slug: slug, Body: body}
		p.Meta.Type = "entity"
		p.Meta.Title = slug
		p.Meta.Created, p.Meta.Updated = "2026-07-27", "2026-07-27"
		return p
	}
	// alpha links to beta and to a page that does not exist; beta links back.
	if err := s.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, RunID: "run-graph",
		UpsertPages: []wiki.Page{
			page("alpha", "# Alpha\n\nSee [[beta]] and [[ghost]]."),
			page("beta", "# Beta\n\nBack to [[alpha]]."),
		},
	}); err != nil {
		t.Fatal(err)
	}

	nodes, edges, err := s.Graph(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	// Only resolved links are edges: the ghost link is a gap, not an edge.
	if len(edges) != 2 {
		t.Fatalf("edges = %+v, want alpha<->beta only", edges)
	}
	seen := map[string]bool{}
	for _, e := range edges {
		seen[e.From+">"+e.To] = true
	}
	if !seen["alpha>beta"] || !seen["beta>alpha"] {
		t.Errorf("edges = %+v", edges)
	}
	// Inbound counts feed node sizing.
	for _, n := range nodes {
		if n.Links != 1 {
			t.Errorf("node %s inbound = %d, want 1", n.Slug, n.Links)
		}
	}
}
