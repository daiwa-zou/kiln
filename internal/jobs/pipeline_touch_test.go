package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// notesKey is the uploaded document the gate drops in twoModuleRequest.
var notesKey = diff.DocKey(diff.UploadOrigin("notes.md"))

// twoModuleRequest is a build in which the ripple unit and an uploaded
// document (with a section) hash as they did last run and the eddy unit does
// not: three for the gate to drop, one to regenerate.
func twoModuleRequest(t *testing.T, store *memStore, runner *scriptedRunner) BuildRequest {
	t.Helper()
	store.sources[diff.ModuleKey("ripple")] = diff.SourceRecord{
		Key: diff.ModuleKey("ripple"), InputHash: "hash-v1", FilesWritten: []string{"entities/ripple.md"},
	}
	store.sources[diff.ModuleKey("eddy")] = diff.SourceRecord{Key: diff.ModuleKey("eddy"), InputHash: "hash-v1"}
	store.sources[notesKey] = diff.SourceRecord{Key: notesKey, InputHash: "hash-v1", FilesWritten: []string{"sources/notes.md"}}
	store.sources[notesKey+"#intro"] = diff.SourceRecord{Key: notesKey + "#intro", InputHash: "hash-v1"}
	runner.filesByAttempt[0] = map[string]string{"entities/eddy.md": validPage("entity", "Eddy")}

	m := testMap(
		mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "hash-v1"},
		mapper.Unit{Key: "module:eddy", Slug: "eddy", Hash: "hash-v2"},
		mapper.Unit{Key: string(notesKey), Slug: "notes", Hash: "hash-v1"},
		mapper.Unit{Key: string(notesKey) + "#intro", Kind: "doc-section", Slug: "notes-intro", Hash: "hash-v1"},
	)
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
		{Path: "apps/eddy/main.go", Kind: diff.Modified},
		{Path: diff.UploadOrigin("notes.md"), Kind: diff.Modified},
	}})
	req.Router.ModuleDirs["apps/eddy"] = diff.ModuleKey("eddy")
	req.Router.DocPaths = map[string]diff.Key{diff.UploadOrigin("notes.md"): notesKey}
	req.Router.DocSections = map[diff.Key][]diff.Key{notesKey: {notesKey + "#intro"}}
	return req
}

// An uploaded document the gate drops was read and found unchanged; that is
// recorded on its source, because the per-document ingest status reads
// fetched_at and nothing else would move it. Only the document's own key is
// stamped: the Ingest page joins on it alone, so the module and the section
// the gate dropped beside it would be rewritten for no reader. A unit that
// regenerates is stamped by its own import.
func TestBuildStampsUnitsTheGateDroppedAsRead(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	req := twoModuleRequest(t, store, runner)

	res, err := testPipeline(store, runner).Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (violations: %v)", res.Summary.Status, res.Violations)
	}
	if want := [][]string{{string(notesKey)}}; !reflect.DeepEqual(store.touched, want) {
		t.Errorf("TouchSources calls = %v, want %v: the dropped document once, not its section, not the module, not the regenerated unit", store.touched, want)
	}
	// The regenerated unit's source is written by the import, as before.
	if imp := store.lastImport(); imp == nil || len(imp.UpsertSources) != 1 || imp.UpsertSources[0].Key != diff.ModuleKey("eddy") {
		t.Errorf("imported sources = %+v, want eddy alone", store.lastImport())
	}
}

// A section is told apart by its unit kind, not by the '#' in its key: a
// stored path may carry one itself, and such a document, dropped by the gate
// as unchanged, must be stamped like any other or it reads as never read after
// every same-text re-upload.
func TestBuildStampsAnUploadWhosePathContainsAHash(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	key := diff.DocKey(diff.UploadOrigin("C# notes.md"))
	store.sources[key] = diff.SourceRecord{Key: key, InputHash: "hash-v1", FilesWritten: []string{"sources/c-notes.md"}}
	store.sources[diff.ModuleKey("eddy")] = diff.SourceRecord{Key: diff.ModuleKey("eddy"), InputHash: "hash-v1"}
	runner.filesByAttempt[0] = map[string]string{"entities/eddy.md": validPage("entity", "Eddy")}
	m := testMap(
		mapper.Unit{Key: "module:eddy", Slug: "eddy", Hash: "hash-v2"},
		mapper.Unit{Key: string(key), Kind: "doc", Slug: "c-notes", Hash: "hash-v1"},
	)
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/eddy/main.go", Kind: diff.Modified},
		{Path: diff.UploadOrigin("C# notes.md"), Kind: diff.Modified},
	}})
	req.Router.ModuleDirs["apps/eddy"] = diff.ModuleKey("eddy")
	req.Router.DocPaths = map[string]diff.Key{diff.UploadOrigin("C# notes.md"): key}

	if _, err := testPipeline(store, runner).Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := [][]string{{string(key)}}; !reflect.DeepEqual(store.touched, want) {
		t.Errorf("TouchSources calls = %v, want %v", store.touched, want)
	}
}

// A dry run previews; it reads nothing of record.
func TestDryRunDoesNotStampUnchangedUnits(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	req := twoModuleRequest(t, store, runner)
	req.DryRun = true

	if _, err := testPipeline(store, runner).Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(store.touched) != 0 {
		t.Errorf("a dry run stamped sources as read: %v", store.touched)
	}
}

// The stamp is bookkeeping for the Ingest page: a store that cannot take it
// must not cost the build.
func TestBuildSurvivesAFailedReadStamp(t *testing.T) {
	store := newMemStore()
	store.failTouch = errors.New("db down")
	runner := newScriptedRunner()
	req := twoModuleRequest(t, store, runner)

	res, err := testPipeline(store, runner).Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build failed on a read stamp: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Summary.Status)
	}
}
