package api

import (
	"net/http"
	"testing"
)

type graphJSON struct {
	Nodes []struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
		Type  string `json:"type"`
		Links int    `json:"links"`
	} `json:"nodes"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"edges"`
	TotalPages int `json:"totalPages"`
}

func TestGraphReturnsNodesEdgesAndTheRealTotal(t *testing.T) {
	srv, js, wsID := testServer(t)
	seed(t, js, wsID)

	var g graphJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/graph", &g); code != http.StatusOK {
		t.Fatalf("GET graph = %d", code)
	}
	if len(g.Nodes) != 2 || g.TotalPages != 2 {
		t.Fatalf("nodes=%d total=%d, want 2 and 2: %+v", len(g.Nodes), g.TotalPages, g)
	}

	// architecture links to ripple; ripple links to a page nobody wrote. Only
	// the resolved one is an edge -- the other is a gap.
	if len(g.Edges) != 1 {
		t.Fatalf("edges = %+v, want architecture -> ripple only", g.Edges)
	}
	if g.Edges[0].From != "architecture" || g.Edges[0].To != "ripple" {
		t.Errorf("edge = %+v, want architecture -> ripple", g.Edges[0])
	}

	byslug := map[string]int{}
	for _, n := range g.Nodes {
		byslug[n.Slug] = n.Links
	}
	if byslug["ripple"] != 1 {
		t.Errorf("ripple inbound = %d, want 1 (the graph sizes hubs by it)", byslug["ripple"])
	}
	if byslug["architecture"] != 0 {
		t.Errorf("architecture inbound = %d, want 0", byslug["architecture"])
	}
}

func TestGraphCapKeepsTheTotalHonestAndDropsDanglingEdges(t *testing.T) {
	srv, js, wsID := testServer(t)
	seed(t, js, wsID)

	// One node of two: the surviving node's edge has an endpoint that did not
	// make the cut, and a line to nowhere is worse than no line.
	var g graphJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/graph?limit=1", &g); code != http.StatusOK {
		t.Fatalf("GET graph = %d", code)
	}
	if len(g.Nodes) != 1 {
		t.Fatalf("nodes = %d, want the cap of 1", len(g.Nodes))
	}
	if g.TotalPages != 2 {
		t.Errorf("totalPages = %d, want 2 -- the cap must not hide how much was left out", g.TotalPages)
	}
	if len(g.Edges) != 0 {
		t.Errorf("edges = %+v, want none: both endpoints have to survive the cap", g.Edges)
	}
}

func TestGraphIsEmptyRatherThanNullOnAnUnbuiltBench(t *testing.T) {
	// Nothing imported at all. The UI iterates both arrays, so null would be a
	// client-side crash where an empty graph is simply an empty canvas.
	srv, _, _ := testServer(t)

	var g graphJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/graph", &g); code != http.StatusOK {
		t.Fatalf("GET graph = %d", code)
	}
	if g.Nodes == nil || g.Edges == nil {
		t.Errorf("graph = %+v, want empty arrays rather than nulls", g)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 || g.TotalPages != 0 {
		t.Errorf("graph on an unbuilt bench = %+v, want empty", g)
	}
}

func TestGraphIsCachedOnTheWikiRevision(t *testing.T) {
	// Links change only on import, so the graph rides the same ETag as every
	// other content read.
	srv, js, wsID := testServer(t)
	seed(t, js, wsID)

	res, err := http.Get(srv.URL + "/api/v1/workspaces/demo/graph")
	if err != nil {
		t.Fatalf("GET graph: %v", err)
	}
	defer res.Body.Close()
	tag := res.Header.Get("ETag")
	if tag == "" {
		t.Fatal("graph carried no ETag")
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workspaces/demo/graph", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", tag)
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET graph: %v", err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET graph = %d, want 304", again.StatusCode)
	}
}
