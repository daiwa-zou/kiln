package jobs

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
)

// These tests cover builds with no repository at all: a bench fed only by
// uploaded documents (or web pages) must build, and must not treat the git
// namespaces it never synced as vanished.

func TestExecuteDocsOnlyDryRunPlansDocsWithoutArch(t *testing.T) {
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"notes.md":  "# Notes\n\nUploaded document body.\n",
		"guide.txt": "A plain-text guide to the kiln.\n",
	})

	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	var progress bytes.Buffer
	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-docs-only",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source:      SourceSpec{DocsDir: docs, Slug: "bench"},
		DryRun:      true,
		Progress:    &progress,
	})
	if err != nil {
		t.Fatalf("Execute without a repository: %v", err)
	}

	if len(res.Planned) == 0 {
		t.Fatal("docs-only dry run planned nothing")
	}
	var sawNotes bool
	for _, k := range res.Planned {
		if k == diff.ArchOverview {
			t.Errorf("plan contains %s; no modules means nothing to synthesize", k)
		}
		if k.Prefix() == "module" {
			t.Errorf("plan contains module unit %s with no repository", k)
		}
		if k == diff.DocKey(diff.UploadOrigin("notes.md")) {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Errorf("uploaded doc missing from plan: %v", res.Planned)
	}
	if strings.Contains(progress.String(), "syncing via git connector") {
		t.Error("progress narrates a git sync that must not have happened")
	}
}

func TestExecuteNothingToBuildFails(t *testing.T) {
	p := testPipeline(newMemStore(), newScriptedRunner())
	_, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r", WorkspaceID: "ws-1", Trigger: "manual",
		Source: SourceSpec{Slug: "bench"},
		DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "nothing to build") {
		t.Fatalf("empty source spec: err = %v, want nothing-to-build", err)
	}
}

func TestExecuteDocsOnlyLeavesRepoSourcesAlone(t *testing.T) {
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"notes.md": "# Notes\n\nUploaded document body.\n",
	})

	store := newMemStore()
	// A repo-derived source on record: this run will not scan any repository,
	// so its absence from the map is ignorance, not disappearance.
	store.sources[diff.Key("module:legacy")] = diff.SourceRecord{
		Key: diff.Key("module:legacy"), InputHash: "old",
		FilesWritten: []string{"entities/legacy.md"},
	}
	// An uploaded doc on record that is gone from the docs dir: that namespace
	// WAS synced, so this one is a real disappearance.
	store.sources[diff.DocKey(diff.UploadOrigin("gone.md"))] = diff.SourceRecord{
		Key: diff.DocKey(diff.UploadOrigin("gone.md")), InputHash: "old",
		FilesWritten: []string{"docs/gone.md"},
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		string(diff.DocKey(diff.UploadOrigin("notes.md"))): {{
			Path: "docs/notes.md", Type: "source", Title: "Notes",
			Body: "# Notes\n\nAn uploaded document describing how the kiln fires " +
				"raw material into something durable across repeated runs.\n",
		}},
	}}
	p := testPipeline(store, runner)

	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-docs-full",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source:      SourceSpec{DocsDir: docs, Slug: "bench"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, rev := range store.deletionReviews {
		if rev.Key == diff.Key("module:legacy") {
			t.Fatal("docs-only run flagged a repo source as disappeared")
		}
	}
	var sawGone bool
	for _, rev := range store.deletionReviews {
		if rev.Key == diff.DocKey(diff.UploadOrigin("gone.md")) {
			sawGone = true
		}
	}
	if !sawGone {
		t.Fatalf("deletion reviews = %+v, want one for the vanished upload", store.deletionReviews)
	}
}

func TestPausedDocumentIsNotFlaggedAsDeleted(t *testing.T) {
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"active.md": "# Active\n\nStill part of the bench.\n",
	})

	store := newMemStore()
	// A previously ingested document, now paused: absent from this sync's
	// map and staging, present in the source records, and named skipped.
	pausedKey := diff.DocKey(diff.UploadOrigin("paused.md"))
	store.sources[pausedKey] = diff.SourceRecord{
		Key: pausedKey, InputHash: "old",
		FilesWritten: []string{"docs/paused.md"},
	}
	// Its section unit rides along.
	sectionKey := diff.Key(string(pausedKey) + "#chapter-one")
	store.sources[sectionKey] = diff.SourceRecord{
		Key: sectionKey, InputHash: "old",
		FilesWritten: []string{"docs/paused-chapter-one.md"},
	}

	runner := &structuredRunner{byUnit: map[string][]agent.GeneratedPage{
		string(diff.DocKey(diff.UploadOrigin("active.md"))): {{
			Path: "docs/active.md", Type: "source", Title: "Active",
			Body: "# Active\n\nAn uploaded document still part of the bench, " +
				"read on every ingest and kept current with its source file.\n",
		}},
	}}
	p := testPipeline(store, runner)

	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-paused-doc",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source: SourceSpec{
			DocsDir: docs, Slug: "bench",
			SkippedKeys: []diff.Key{pausedKey},
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(store.deletionReviews) != 0 {
		t.Fatalf("deletion reviews = %+v; a paused document must not read as deleted", store.deletionReviews)
	}
	// The paused document's pages survive untouched.
	if _, ok := store.sources[pausedKey]; !ok {
		t.Error("paused document's source record was dropped")
	}
}
