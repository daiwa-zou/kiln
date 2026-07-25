package docmap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

func testMapper() *Mapper {
	return &Mapper{Now: func() time.Time { return time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC) }}
}

func doc(key, path, title, text string) Doc {
	return Doc{Key: key, Path: path, Title: title, Text: text, Hash: hashText(text)}
}

func TestMapDocsOneUnitPerDocument(t *testing.T) {
	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:q3.pdf", "q3.pdf", "Q3 Report", "Short body."),
		doc("doc:notes.md", "notes.md", "Notes", "Also short."),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}

	if len(wm.Units) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(wm.Units), wm.Units)
	}
	if wm.Kind != "doc" {
		t.Errorf("Kind = %q", wm.Kind)
	}
	for _, u := range wm.Units {
		if u.Hash == "" || u.Slug == "" || u.Key == "" {
			t.Errorf("incomplete unit: %+v", u)
		}
	}
}

func TestMapDocsSplitsLongDocuments(t *testing.T) {
	// A long document with several top-level headings becomes one unit per
	// section, so editing one chapter regenerates one section rather than the
	// whole book.
	body := "# Introduction\n" + strings.Repeat("intro prose. ", 1200) +
		"\n# Methods\n" + strings.Repeat("method prose. ", 1200) +
		"\n# Results\n" + strings.Repeat("result prose. ", 1200)

	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:paper.pdf", "paper.pdf", "The Paper", body),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}

	// One whole-document unit plus one per section.
	if len(wm.Units) != 4 {
		t.Fatalf("got %d units, want 4: %v", len(wm.Units), unitKeys(wm))
	}

	var sections []string
	for _, u := range wm.Units {
		if u.Kind == "doc-section" {
			sections = append(sections, u.Title)
		}
	}
	want := []string{"Introduction", "Methods", "Results"}
	if len(sections) != 3 {
		t.Fatalf("sections = %v, want %v", sections, want)
	}

	// Sections must hash independently, or splitting buys nothing: any edit
	// would dirty every section at once.
	hashes := map[string]bool{}
	for _, u := range wm.Units {
		if u.Kind == "doc-section" {
			if hashes[u.Hash] {
				t.Error("two sections share a hash; they would regenerate together")
			}
			hashes[u.Hash] = true
		}
	}
}

func TestMapDocsDoesNotSplitShortDocuments(t *testing.T) {
	body := "# One\n\nShort.\n\n# Two\n\nAlso short.\n"

	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:short.md", "short.md", "Short", body),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}
	// Splitting a short document adds units without adding value.
	if len(wm.Units) != 1 {
		t.Errorf("a short document was split into %d units", len(wm.Units))
	}
}

func TestMapDocsDoesNotSplitOnASingleHeading(t *testing.T) {
	// One heading means the section and the document are the same thing;
	// splitting would just duplicate it under a new name.
	body := "# Only Heading\n" + strings.Repeat("prose. ", 5000)

	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:long.md", "long.md", "Long", body),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}
	if len(wm.Units) != 1 {
		t.Errorf("got %d units, want 1: %v", len(wm.Units), unitKeys(wm))
	}
}

func TestMapDocsLinksSectionsToTheirDocument(t *testing.T) {
	body := "# A\n" + strings.Repeat("a ", 6000) + "\n# B\n" + strings.Repeat("b ", 6000)

	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:paper.pdf", "paper.pdf", "Paper", body),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}

	if len(wm.Edges) != 2 {
		t.Fatalf("edges = %+v, want one per section", wm.Edges)
	}
	for _, e := range wm.Edges {
		if e.To != "doc:paper.pdf" {
			t.Errorf("edge points at %q, want the parent document", e.To)
		}
	}
}

func TestMapDocsSlugsAreUnique(t *testing.T) {
	// Two documents in different folders can share a basename; colliding slugs
	// would silently overwrite one page with the other.
	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		doc("doc:a/report.pdf", "a/report.pdf", "Report", "x"),
		doc("doc:b/report.pdf", "b/report.pdf", "Report", "y"),
	})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}

	seen := map[string]bool{}
	for _, u := range wm.Units {
		if seen[u.Slug] {
			t.Errorf("duplicate slug %q", u.Slug)
		}
		seen[u.Slug] = true
	}
}

func TestMapDocsIsDeterministic(t *testing.T) {
	docs := []Doc{
		doc("doc:z.md", "z.md", "Z", "zzz"),
		doc("doc:a.md", "a.md", "A", "aaa"),
		doc("doc:m.md", "m.md", "M", "mmm"),
	}

	first, err := testMapper().MapDocs(context.Background(), "/docs", docs)
	if err != nil {
		t.Fatal(err)
	}

	// Reversing input order must not change the map, or every run would look
	// like it rewrote everything.
	reversed := make([]Doc, len(docs))
	for i, d := range docs {
		reversed[len(docs)-1-i] = d
	}
	second, err := testMapper().MapDocs(context.Background(), "/docs", reversed)
	if err != nil {
		t.Fatal(err)
	}

	if first.Hash != second.Hash {
		t.Error("map hash depends on input order")
	}
	for i := range first.Units {
		if first.Units[i].Key != second.Units[i].Key {
			t.Errorf("unit %d differs: %q vs %q", i, first.Units[i].Key, second.Units[i].Key)
		}
	}
}

func TestMapDocsFallsBackToFilenameTitle(t *testing.T) {
	wm, err := testMapper().MapDocs(context.Background(), "/docs", []Doc{
		{Key: "doc:untitled.md", Path: "reports/untitled.md", Text: "body", Hash: "h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wm.Units[0].Title != "untitled" {
		t.Errorf("Title = %q, want the filename", wm.Units[0].Title)
	}
}

func TestMapperSatisfiesTheInterface(t *testing.T) {
	// The whole reason docmap exists alongside repomap: a Mapper interface with
	// one implementation is an assumption, not a seam.
	var _ mapper.Mapper = (*Mapper)(nil)
}

func TestMapViaSourceSet(t *testing.T) {
	// Map reads the staged text off disk -- that read is what lets a long
	// document split into sections on the generic path.
	root := t.TempDir()
	long := "# One\n\n" + strings.Repeat("alpha content here. ", 800) +
		"\n\n# Two\n\n" + strings.Repeat("beta content here. ", 800)
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}

	set := &mapper.SourceSet{
		Root: root,
		Kind: "upload",
		Items: []mapper.SourceItem{
			{Key: "doc:a.md", Path: "a.md", Title: "A", Hash: "h1"},
		},
	}

	wm, err := testMapper().Map(context.Background(), set)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(wm.Units) != 3 || wm.Units[0].Key != "doc:a.md" {
		t.Errorf("units = %+v, want the whole doc plus two sections", unitKeys(wm))
	}

	// A missing staged file is an error, not a silently empty document.
	set.Items[0].Path = "missing.md"
	if _, err := testMapper().Map(context.Background(), set); err == nil {
		t.Error("Map succeeded with unreadable staged text")
	}
}

func unitKeys(wm *mapper.WorkspaceMap) []string {
	out := make([]string, 0, len(wm.Units))
	for _, u := range wm.Units {
		out = append(out, u.Key)
	}
	return out
}
