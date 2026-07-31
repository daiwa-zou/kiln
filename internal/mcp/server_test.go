package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHighlightToMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no markers", "plain text", "plain text"},
		{"one match", "the [[[budget]]] ledger", "the **budget** ledger"},
		{"several matches", "[[[a]]] and [[[b]]]", "**a** and **b**"},
		// ts_headline truncates fragments, so a snippet can end part-way
		// through a marker. Bolding from there would run to the end of the
		// snippet, so the stray marker is dropped instead.
		{"unbalanced opener", "trailing [[[budget", "trailing budget"},
		{"unbalanced closer", "budget]]] leading", "budget leading"},
		// The reason this rewrite exists: [[[x]]] handed to a model reads as a
		// wikilink to a page named x.
		{"no wikilink survives", "unit `[[[module]]]:cmd-kiln`", "unit `**module**:cmd-kiln`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := highlightToMarkdown(tc.in); got != tc.want {
				t.Errorf("highlightToMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(highlightToMarkdown(tc.in), "[[") {
				t.Errorf("output still contains wikilink-shaped brackets: %q", highlightToMarkdown(tc.in))
			}
		})
	}
}

func TestBenchForPrefersArgumentThenDefault(t *testing.T) {
	withDefault := NewServer(nil, "configured", "test")
	if got, err := withDefault.benchFor("explicit"); err != nil || got != "explicit" {
		t.Errorf("benchFor(explicit) = %q, %v; the argument must win", got, err)
	}
	if got, err := withDefault.benchFor("  "); err != nil || got != "configured" {
		t.Errorf("benchFor(blank) = %q, %v; want the configured default", got, err)
	}

	none := NewServer(nil, "", "test")
	_, err := none.benchFor("")
	if err == nil {
		t.Fatal("a call with no bench and no default must not silently guess one")
	}
	// The message has to say how to recover, since this is the first thing an
	// agent hits on a multi-bench instance.
	if !strings.Contains(err.Error(), "list_benches") {
		t.Errorf("error does not point at a way forward: %v", err)
	}
}

// fakeKiln serves just enough of the read API to drive the tools.
func fakeKiln(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []Workspace{
			{Slug: "kiln", Name: "kiln", PageCount: 2},
			{Slug: "empty", Name: "Empty", PageCount: 0},
		})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "nothing" {
			writeJSON(w, []SearchHit{})
			return
		}
		writeJSON(w, []SearchHit{{
			Path: "entities/jobs.md", Slug: "jobs", Type: "entity",
			Title: "internal/jobs", Snippet: "the [[[budget]]] ledger",
		}})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/pages/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, Page{
			Path: "entities/jobs.md", Slug: "jobs", Type: "entity",
			Title: "internal/jobs", Updated: "2026-07-30", BuiltAtRef: "abc1234",
			Related: []string{"pipeline"}, Body: "# internal/jobs\n\nThe pipeline.",
		})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/gaps", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []Gap{{Slug: "missing-page", WantedBy: []string{"jobs"}}})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/pages", func(w http.ResponseWriter, r *http.Request) {
		// Echo the paging arguments back as a page, so a test can prove the
		// tool actually forwarded them.
		writeJSON(w, []PageSummary{
			{Path: "entities/jobs.md", Slug: "jobs", Type: "entity", Title: "internal/jobs"},
			{
				Path: "concepts/limit.md", Slug: "limit-" + r.URL.Query().Get("limit"),
				Type: "concept", Title: "offset-" + r.URL.Query().Get("offset"),
			},
		})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/overview", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, artifact{Kind: "overview", Body: "# Overview\n\nKnowledge base for kiln."})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/index", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, artifact{Kind: "index", Body: "# Wiki Index\n\n## Entities\n\n- internal/jobs"})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/backlinks/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []PageSummary{{Path: "concepts/pipeline.md", Slug: "pipeline", Type: "concept", Title: "Pipeline"}})
	})
	mux.HandleFunc("/api/v1/workspaces/kiln/backlinks/orphan", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []PageSummary{})
	})
	// Everything else is a 404, which is also how kiln answers for a bench the
	// caller cannot see.
	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// call runs one tool and returns its text plus whether it reported an error.
func call(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "kiln", Version: "test"}, nil)
	s.Register(srv)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ct, st := mcp.NewInMemoryTransports()

	ctx := context.Background()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		ss, err := srv.Connect(ctx, st, nil)
		if err != nil {
			return
		}
		_ = ss.Wait()
	}()

	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close(); <-serverDone }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestToolsReadTheWiki(t *testing.T) {
	api := fakeKiln(t)
	defer api.Close()
	s := NewServer(NewClient(api.URL, ""), "kiln", "test")

	t.Run("list_benches flags an unbuilt bench", func(t *testing.T) {
		out, isErr := call(t, s, "list_benches", map[string]any{})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		if !strings.Contains(out, "kiln") || !strings.Contains(out, "empty") {
			t.Errorf("benches missing from output:\n%s", out)
		}
		// A bench with no pages searches empty for reasons unrelated to the
		// query, so the listing has to say so.
		if !strings.Contains(out, "has not been built") {
			t.Errorf("an empty bench was not called out:\n%s", out)
		}
	})

	t.Run("search rewrites highlight markers", func(t *testing.T) {
		out, isErr := call(t, s, "search_wiki", map[string]any{"query": "budget"})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		if !strings.Contains(out, "**budget**") {
			t.Errorf("highlight not rewritten to markdown:\n%s", out)
		}
		if strings.Contains(out, "[[") {
			t.Errorf("output contains wikilink-shaped brackets:\n%s", out)
		}
		// The slug is the handle the agent needs for read_page.
		if !strings.Contains(out, "jobs") {
			t.Errorf("slug missing, so the hit cannot be followed:\n%s", out)
		}
	})

	t.Run("empty search suggests a way forward", func(t *testing.T) {
		out, isErr := call(t, s, "search_wiki", map[string]any{"query": "nothing"})
		if isErr {
			t.Fatalf("no matches is not an error: %s", out)
		}
		if !strings.Contains(out, "wiki_gaps") {
			t.Errorf("no-results message does not suggest anything:\n%s", out)
		}
	})

	t.Run("read_page carries provenance", func(t *testing.T) {
		out, isErr := call(t, s, "read_page", map[string]any{"page": "jobs"})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		for _, want := range []string{"The pipeline.", "abc1234", "pipeline"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("missing page is a recoverable error", func(t *testing.T) {
		out, isErr := call(t, s, "read_page", map[string]any{"page": "ghost"})
		if !isErr {
			t.Fatal("a missing page should report as a tool error")
		}
		if !strings.Contains(out, "search_wiki") {
			t.Errorf("error does not say how to find the right slug:\n%s", out)
		}
	})

	t.Run("unknown bench names the recovery", func(t *testing.T) {
		out, isErr := call(t, s, "search_wiki", map[string]any{"query": "x", "bench": "ghost"})
		if !isErr {
			t.Fatal("an unknown bench should report as a tool error")
		}
		if !strings.Contains(out, "list_benches") {
			t.Errorf("error does not point at list_benches:\n%s", out)
		}
	})

	t.Run("overview joins the two artifacts", func(t *testing.T) {
		out, isErr := call(t, s, "wiki_overview", map[string]any{})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		// Orientation needs both: what the bench is about, and what is in it.
		if !strings.Contains(out, "Knowledge base for kiln") {
			t.Errorf("overview body missing:\n%s", out)
		}
		if !strings.Contains(out, "Wiki Index") {
			t.Errorf("index missing, so the agent cannot see what pages exist:\n%s", out)
		}
	})

	t.Run("list_pages forwards paging and offers the next page", func(t *testing.T) {
		out, isErr := call(t, s, "list_pages", map[string]any{"limit": 2, "offset": 10})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		// The fake echoes the arguments into the row it returns, which is how
		// this proves they were actually sent rather than defaulted.
		if !strings.Contains(out, "limit-2") || !strings.Contains(out, "offset-10") {
			t.Errorf("paging arguments were not forwarded:\n%s", out)
		}
		// A full page means there may be more; saying so beats the agent
		// assuming it has seen the whole bench.
		if !strings.Contains(out, "offset=12") {
			t.Errorf("no continuation offered on a full page:\n%s", out)
		}
	})

	t.Run("backlinks lists referrers, and says so when there are none", func(t *testing.T) {
		out, isErr := call(t, s, "page_backlinks", map[string]any{"page": "jobs"})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		if !strings.Contains(out, "pipeline") {
			t.Errorf("referring page missing:\n%s", out)
		}

		out, isErr = call(t, s, "page_backlinks", map[string]any{"page": "orphan"})
		if isErr {
			t.Fatalf("no backlinks is not an error: %s", out)
		}
		if !strings.Contains(out, "Nothing") {
			t.Errorf("empty backlinks should say so plainly:\n%s", out)
		}
	})

	t.Run("gaps distinguishes absent from uncovered", func(t *testing.T) {
		out, isErr := call(t, s, "wiki_gaps", map[string]any{})
		if isErr {
			t.Fatalf("unexpected error: %s", out)
		}
		if !strings.Contains(out, "missing-page") || !strings.Contains(out, "jobs") {
			t.Errorf("gap and its wanter missing:\n%s", out)
		}
	})
}

func TestUnreachableKilnSaysWhatToCheck(t *testing.T) {
	// A closed port is the overwhelmingly likely failure, and a bare dial
	// error reads to an agent as "the wiki is broken".
	api := fakeKiln(t)
	url := api.URL
	api.Close()

	s := NewServer(NewClient(url, ""), "kiln", "test")
	out, isErr := call(t, s, "list_benches", map[string]any{})
	if !isErr {
		t.Fatal("an unreachable kiln should report as a tool error")
	}
	if !strings.Contains(out, "cannot reach kiln") {
		t.Errorf("error does not name the cause:\n%s", out)
	}
}
