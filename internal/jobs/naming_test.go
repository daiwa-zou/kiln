package jobs

import (
	"bytes"
	"context"
	"testing"
)

// The whole point of naming at ingest rather than at upload: by the time a
// build runs, the document has been read, and what it calls itself beats
// whatever the scanner called the file.
//
// This covers the wiring rather than the derivation -- naming has its own
// tests. What is easy to get wrong here is the plumbing between them: the
// connector derives a name, and it has to survive the mapper, the merge and
// the request that reaches the store.
func TestExecuteRenamesDocumentsFromTheirContents(t *testing.T) {
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		// A filename that says nothing, over a document that says what it is.
		"Scan_2026-03-14_11-42-08.md": "# Board Diversity Disclosure Policy\n\nThis policy sets out...\n",
		// No heading: the filename is all there is, and it still gets tidied.
		"NBAE5150_syllabus_v3_FINAL.md": "Week 1: introductions\n",
	})

	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	var progress bytes.Buffer
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-naming",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source:      SourceSpec{DocsDir: docs, Slug: "bench"},
		Progress:    &progress,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := map[string]string{
		"Scan_2026-03-14_11-42-08.md":   "Board Diversity Disclosure Policy",
		"NBAE5150_syllabus_v3_FINAL.md": "NBAE5150 Syllabus",
	}
	for path, name := range want {
		if got := store.renamed[path]; got != name {
			t.Errorf("document %q named %q, want %q", path, got, name)
		}
	}
}

// A dry run is a question, not a change. It must not rename anything, for the
// same reason it does not write pages.
func TestExecuteDryRunRenamesNothing(t *testing.T) {
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"Scan_2026-03-14.md": "# Board Diversity Disclosure Policy\n\nBody.\n",
	})

	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	var progress bytes.Buffer
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-dry",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source:      SourceSpec{DocsDir: docs, Slug: "bench"},
		DryRun:      true,
		Progress:    &progress,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.renamed) != 0 {
		t.Errorf("a dry run renamed %v", store.renamed)
	}
}
