package wiki

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// assertGolden compares got against testdata/golden/<name>, rewriting it when
// -update is passed. The emitters are the format contract with everything that
// reads a kiln wiki, so they get byte-exact goldens rather than shape assertions.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", "golden", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./internal/wiki -update)", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// samplePages is a small wiki covering every section, mixed-case titles, and a
// page whose title matches its slug.
func samplePages() []Page {
	return []Page{
		{
			Path: "entities/module-apps-ripple.md", Slug: "module-apps-ripple",
			Meta: Frontmatter{Type: TypeEntity, Title: "apps/ripple", Updated: "2026-07-22"},
		},
		{
			Path: "entities/module-apps-beacon.md", Slug: "module-apps-beacon",
			Meta: Frontmatter{Type: TypeEntity, Title: "apps/beacon", Updated: "2026-07-24"},
		},
		{
			Path: "entities/uspto.md", Slug: "uspto",
			Meta: Frontmatter{Type: TypeEntity, Title: "uspto", Updated: "2026-07-20"},
		},
		{
			Path: "concepts/task-dispatch.md", Slug: "task-dispatch",
			Meta: Frontmatter{Type: TypeConcept, Title: "Task Dispatch", Updated: "2026-07-25"},
		},
		{
			Path: "concepts/backpressure.md", Slug: "backpressure",
			Meta: Frontmatter{Type: TypeConcept, Title: "Backpressure", Updated: "2026-07-21"},
		},
		{
			Path: "sources/readme.md", Slug: "readme",
			Meta: Frontmatter{Type: TypeSource, Title: "README.md", Updated: "2026-07-19"},
		},
		{
			Path: "synthesis/architecture-overview.md", Slug: "architecture-overview",
			Meta: Frontmatter{Type: TypeSynthesis, Title: "Architecture Overview", Updated: "2026-07-25"},
		},
		{
			Path: "comparisons/ripple-vs-beacon.md", Slug: "ripple-vs-beacon",
			Meta: Frontmatter{Type: TypeComparison, Title: "ripple vs beacon", Updated: "2026-07-23"},
		},
		{
			Path: "queries/who-owns-retries.md", Slug: "who-owns-retries",
			Meta: Frontmatter{Type: TypeQuery, Title: "Who owns retry policy?", Updated: "2026-07-18"},
		},
	}
}

func TestBuildIndexGolden(t *testing.T) {
	assertGolden(t, "index.md", BuildIndex(samplePages(), IndexOptions{RecentLimit: 5}))
}

func TestBuildIndexEmptyGolden(t *testing.T) {
	// Every section is emitted even with no pages, so the file's shape is
	// stable and a diff shows only real changes.
	assertGolden(t, "index_empty.md", BuildIndex(nil, IndexOptions{}))
}

func TestBuildIndexExcludesReserved(t *testing.T) {
	pages := append(samplePages(),
		Page{Path: "overview.md", Slug: "overview", Meta: Frontmatter{Type: TypeOverview, Title: "Overview"}},
	)

	got := BuildIndex(pages, IndexOptions{RecentLimit: 5})
	want := BuildIndex(samplePages(), IndexOptions{RecentLimit: 5})

	if got != want {
		t.Error("overview.md leaked into the index; reserved pages must be excluded")
	}
}

func TestBuildIndexIsDeterministic(t *testing.T) {
	pages := samplePages()
	first := BuildIndex(pages, IndexOptions{RecentLimit: 5})

	// Reversing input order must not change output: the index is sorted, and a
	// run that reordered it would look like every page changed.
	for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
		pages[i], pages[j] = pages[j], pages[i]
	}
	if second := BuildIndex(pages, IndexOptions{RecentLimit: 5}); first != second {
		t.Error("BuildIndex is order-dependent; output must be stable regardless of input order")
	}
}

func TestBuildOverviewGolden(t *testing.T) {
	got := BuildOverview(samplePages(), OverviewInput{
		Workspace: "watchtower",
		Ref:       "7dfadb6",
		Date:      "2026-07-25",
		Narrative: []string{
			"Five Go services behind a gateway, each owning a slice of the request path.",
		},
	})
	assertGolden(t, "overview.md", got)
}

func TestBuildOverviewEmpty(t *testing.T) {
	got := BuildOverview(nil, OverviewInput{Workspace: "empty", Date: "2026-07-25"})

	// A wiki with no pages must still produce a valid overview rather than a
	// half-rendered frame.
	if !strings.Contains(got, "no pages yet") {
		t.Errorf("empty overview should say so:\n%s", got)
	}
	if _, _, err := ParseFrontmatter([]byte(got)); err != nil {
		t.Errorf("empty overview has invalid frontmatter: %v", err)
	}
}

func TestBuildOverviewIsDeterministic(t *testing.T) {
	pages := samplePages()
	in := OverviewInput{Workspace: "w", Ref: "abc", Date: "2026-07-25"}

	first := BuildOverview(pages, in)
	for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
		pages[i], pages[j] = pages[j], pages[i]
	}
	// Counting pages by type means iterating a map; a leak of that iteration
	// order would make every run look like it rewrote the overview.
	if second := BuildOverview(pages, in); first != second {
		t.Error("BuildOverview output varies with input order")
	}
}

func TestBuildOverviewIsParseable(t *testing.T) {
	got := BuildOverview(samplePages(), OverviewInput{Workspace: "w", Date: "2026-07-25"})

	meta, _, err := ParseFrontmatter([]byte(got))
	if err != nil {
		t.Fatalf("generated overview does not parse: %v", err)
	}
	// The overview is emitted deterministically and must never be mistaken for
	// an agent-writable page.
	if meta.Type != TypeOverview {
		t.Errorf("type = %q, want overview", meta.Type)
	}
	if meta.Type.Generated() {
		t.Error("overview must not be agent-writable")
	}
}

func TestRenderLogEntryGolden(t *testing.T) {
	entry := LogEntry{
		Date:    "2026-07-25",
		Action:  "build",
		Subject: "watchtower",
		Ref:     "7dfadb6",
		Lines: []string{
			"Modules updated: [[entities/module-internal-auth]], [[entities/module-proto]]",
			"Pages: 3 created, 5 updated, 1 removed",
			"Trigger: git commit 7dfadb6",
			"Cost: $0.84 across 4 agent calls (12 turns)",
		},
	}
	assertGolden(t, "log_entry.md", RenderLogEntry(entry))
}

func TestAppendLogEntryGolden(t *testing.T) {
	existing := "# Wiki Log\n\n## [2026-07-24] build | watchtower @ a1b2c3d\n\n- Pages: 2 created\n"

	entry := LogEntry{
		Date: "2026-07-25", Action: "build", Subject: "watchtower", Ref: "7dfadb6",
		Lines: []string{"Pages: 1 updated"},
	}
	assertGolden(t, "log_appended.md", AppendLogEntry(existing, entry))
}

func TestAppendLogEntrySeedsHeader(t *testing.T) {
	entry := LogEntry{Date: "2026-07-25", Action: "build", Subject: "kiln", Lines: []string{"Pages: 1 created"}}

	for _, existing := range []string{"", "\n\n", "   \n"} {
		got := AppendLogEntry(existing, entry)
		if got[:len(LogHeader)] != LogHeader {
			t.Errorf("AppendLogEntry(%q) did not seed the header, got:\n%s", existing, got)
		}
	}
}

func TestSummarizeChanges(t *testing.T) {
	tests := []struct {
		name                      string
		created, updated, deleted int
		want                      string
	}{
		{"all three", 3, 5, 1, "Pages: 3 created, 5 updated, 1 removed"},
		{"zeros omitted", 0, 5, 0, "Pages: 5 updated"},
		{"nothing happened", 0, 0, 0, "No page changes"},
		{"only deletions", 0, 0, 2, "Pages: 2 removed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SummarizeChanges(tt.created, tt.updated, tt.deleted)
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("SummarizeChanges = %v, want [%q]", got, tt.want)
			}
		})
	}
}
