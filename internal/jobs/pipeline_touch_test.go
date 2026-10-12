package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// twoModuleRequest is a build in which the ripple unit hashes as it did last
// run and the eddy unit does not: one for the gate to drop, one to regenerate.
func twoModuleRequest(t *testing.T, store *memStore, runner *scriptedRunner) BuildRequest {
	t.Helper()
	store.sources[diff.ModuleKey("ripple")] = diff.SourceRecord{
		Key: diff.ModuleKey("ripple"), InputHash: "hash-v1", FilesWritten: []string{"entities/ripple.md"},
	}
	store.sources[diff.ModuleKey("eddy")] = diff.SourceRecord{Key: diff.ModuleKey("eddy"), InputHash: "hash-v1"}
	runner.filesByAttempt[0] = map[string]string{"entities/eddy.md": validPage("entity", "Eddy")}

	m := testMap(
		mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "hash-v1"},
		mapper.Unit{Key: "module:eddy", Slug: "eddy", Hash: "hash-v2"},
	)
	req := testRequest(t, m, diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
		{Path: "apps/eddy/main.go", Kind: diff.Modified},
	}})
	req.Router.ModuleDirs["apps/eddy"] = diff.ModuleKey("eddy")
	return req
}

// A unit the gate drops was read and found unchanged; that is recorded on its
// source, because the per-document ingest status reads fetched_at and nothing
// else would move it. A unit that regenerates is stamped by its own import.
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
	if want := [][]string{{"module:ripple"}}; !reflect.DeepEqual(store.touched, want) {
		t.Errorf("TouchSources calls = %v, want %v: the dropped unit once, the regenerated one never", store.touched, want)
	}
	// The regenerated unit's source is written by the import, as before.
	if imp := store.lastImport(); imp == nil || len(imp.UpsertSources) != 1 || imp.UpsertSources[0].Key != diff.ModuleKey("eddy") {
		t.Errorf("imported sources = %+v, want eddy alone", store.lastImport())
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
