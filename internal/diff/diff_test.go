package diff

import (
	"reflect"
	"testing"
)

func TestKeyNamespacing(t *testing.T) {
	tests := []struct {
		key    Key
		prefix string
		id     string
		valid  bool
	}{
		{ModuleKey("apps-ripple"), PrefixModule, "apps-ripple", true},
		{DocKey("reports/q3.pdf"), PrefixDoc, "reports/q3.pdf", true},
		{ArchOverview, PrefixArch, "overview", true},
		{EntryKey("sawmill"), PrefixEntry, "sawmill", true},
		{Key("bare"), "", "bare", false},
		{Key("unknown:x"), "unknown", "x", false},
		{Key("module:"), PrefixModule, "", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			if got := tt.key.Prefix(); got != tt.prefix {
				t.Errorf("Prefix() = %q, want %q", got, tt.prefix)
			}
			if got := tt.key.ID(); got != tt.id {
				t.Errorf("ID() = %q, want %q", got, tt.id)
			}
			if got := tt.key.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestKeysDoNotCollideAcrossNamespaces(t *testing.T) {
	// A document named "overview" must not collide with the architecture
	// synthesis, which is the whole reason keys are namespaced.
	if DocKey("overview") == ArchOverview {
		t.Error("doc:overview collides with arch:overview")
	}
}

func TestHashDiff(t *testing.T) {
	before := map[string]string{
		"a.go":       "h1",
		"b.go":       "h2",
		"removed.go": "h3",
	}
	after := map[string]string{
		"a.go":     "h1",    // unchanged
		"b.go":     "h2new", // modified
		"added.go": "h4",    // added
	}

	cs := HashDiff(before, after)
	want := []Change{
		{Path: "added.go", Kind: Added},
		{Path: "b.go", Kind: Modified},
		{Path: "removed.go", Kind: Deleted},
	}

	if !reflect.DeepEqual(cs.Changes, want) {
		t.Errorf("Changes =\n  %+v\nwant\n  %+v", cs.Changes, want)
	}

	added, modified, deleted := cs.Counts()
	if added != 1 || modified != 1 || deleted != 1 {
		t.Errorf("Counts() = %d, %d, %d; want 1, 1, 1", added, modified, deleted)
	}
}

func TestHashDiffUnchangedIsEmpty(t *testing.T) {
	m := map[string]string{"a.go": "h1", "b.go": "h2"}

	// This is the case that must cost nothing: identical inputs, no LLM call.
	cs := HashDiff(m, m)
	if !cs.Empty() {
		t.Errorf("identical inputs produced changes: %+v", cs.Changes)
	}
}

func TestHashDiffFirstRun(t *testing.T) {
	cs := HashDiff(nil, map[string]string{"a.go": "h1"})
	if !cs.FullRebuild {
		t.Error("FullRebuild = false, want true when there is no baseline")
	}
}

func TestHashDiffIsDeterministic(t *testing.T) {
	before := map[string]string{"z.go": "1", "a.go": "2", "m.go": "3"}
	after := map[string]string{"z.go": "9", "a.go": "9", "m.go": "9"}

	first := HashDiff(before, after)
	for range 20 {
		if got := HashDiff(before, after); !reflect.DeepEqual(got.Changes, first.Changes) {
			t.Fatal("HashDiff output varies between runs; map iteration order is leaking")
		}
	}
}

// flowbitRouter mirrors the flowbit shape: a root module plus two app modules.
func flowbitRouter() Router {
	return Router{
		ModuleDirs: map[string]Key{
			".":           ModuleKey("root"),
			"apps/ripple": ModuleKey("apps-ripple"),
			"apps/beacon": ModuleKey("apps-beacon"),
		},
		DocPaths: map[string]Key{
			"README.md": DocKey("README.md"),
		},
	}
}

func TestRouteAttributesToDeepestModule(t *testing.T) {
	cs := ChangeSet{Changes: []Change{
		{Path: "apps/ripple/internal/task/task.go", Kind: Modified},
	}}

	plan := flowbitRouter().Route(cs)

	if len(plan.Dirty) != 1 || plan.Dirty[0] != ModuleKey("apps-ripple") {
		t.Errorf("Dirty = %v, want only module:apps-ripple", plan.Dirty)
	}
}

func TestRouteManifestDirtiesArchitecture(t *testing.T) {
	// Module boundaries or dependencies may have changed, so the synthesis
	// pages can no longer be trusted.
	cs := ChangeSet{Changes: []Change{{Path: "apps/ripple/go.mod", Kind: Modified}}}

	plan := flowbitRouter().Route(cs)

	if !containsKey(plan.Dirty, ModuleKey("apps-ripple")) {
		t.Errorf("Dirty = %v, want it to include the module", plan.Dirty)
	}
	if !containsKey(plan.Dirty, ArchOverview) {
		t.Errorf("Dirty = %v, want it to include arch:overview", plan.Dirty)
	}
}

func TestRouteArchitecturalFiles(t *testing.T) {
	for _, p := range []string{"docker-compose.yml", "Makefile", "Tiltfile", "proto/auth/auth.proto"} {
		t.Run(p, func(t *testing.T) {
			cs := ChangeSet{Changes: []Change{{Path: p, Kind: Modified}}}
			plan := flowbitRouter().Route(cs)
			if !containsKey(plan.Dirty, ArchOverview) {
				t.Errorf("%s did not dirty arch:overview (got %v)", p, plan.Dirty)
			}
		})
	}
}

func TestRouteDocumentIsIsolated(t *testing.T) {
	cs := ChangeSet{Changes: []Change{{Path: "README.md", Kind: Modified}}}

	plan := flowbitRouter().Route(cs)

	if len(plan.Dirty) != 1 || plan.Dirty[0] != DocKey("README.md") {
		t.Errorf("Dirty = %v, want only the doc key; a doc change must not dirty modules", plan.Dirty)
	}
}

func TestRouteCosmeticFilesAreSkipped(t *testing.T) {
	r := flowbitRouter()
	r.Cosmetic = func(p string) bool { return p == "apps/ripple/NOTES.md" }

	cs := ChangeSet{Changes: []Change{{Path: "apps/ripple/NOTES.md", Kind: Modified}}}
	if plan := r.Route(cs); !plan.IsEmpty() {
		t.Errorf("cosmetic change produced work: %v", plan.Dirty)
	}
}

func TestRouteCosmeticDoesNotMaskManifests(t *testing.T) {
	// A blanket cosmetic rule must never suppress a manifest change.
	r := flowbitRouter()
	r.Cosmetic = func(string) bool { return true }

	cs := ChangeSet{Changes: []Change{{Path: "apps/ripple/go.mod", Kind: Modified}}}
	if plan := r.Route(cs); plan.IsEmpty() {
		t.Error("a manifest change was suppressed by the cosmetic rule")
	}
}

func TestRouteEmptyChangeSetCostsNothing(t *testing.T) {
	if plan := flowbitRouter().Route(ChangeSet{}); !plan.IsEmpty() {
		t.Errorf("empty change set produced work: %v", plan.Dirty)
	}
}

func TestRouteFullRebuildDirtiesEverything(t *testing.T) {
	plan := flowbitRouter().Route(ChangeSet{FullRebuild: true})

	for _, want := range []Key{ModuleKey("root"), ModuleKey("apps-ripple"), ModuleKey("apps-beacon"), DocKey("README.md"), ArchOverview} {
		if !containsKey(plan.Dirty, want) {
			t.Errorf("full rebuild missing %s (got %v)", want, plan.Dirty)
		}
	}
}

func TestRouteOrdersArchitectureLast(t *testing.T) {
	// Architecture summarizes the modules, so it must regenerate after them.
	cs := ChangeSet{Changes: []Change{
		{Path: "apps/ripple/go.mod", Kind: Modified},
		{Path: "apps/beacon/main.go", Kind: Modified},
	}}

	plan := flowbitRouter().Route(cs)
	if plan.Dirty[len(plan.Dirty)-1] != ArchOverview {
		t.Errorf("arch:overview is not last: %v", plan.Dirty)
	}
}

func TestRouteNoRootModule(t *testing.T) {
	// The InfraFlux shape: sibling modules, no root manifest. A file outside
	// every module belongs to none of them.
	r := Router{ModuleDirs: map[string]Key{
		"shared": ModuleKey("shared"),
		"pilot":  ModuleKey("pilot"),
	}}

	cs := ChangeSet{Changes: []Change{{Path: "protobufs/README.md", Kind: Modified}}}
	if plan := r.Route(cs); !plan.IsEmpty() {
		t.Errorf("a file owned by no module produced work: %v", plan.Dirty)
	}
}

func TestStaleAdjacentDoesNotCascade(t *testing.T) {
	// Regenerating everything that links to a changed page would rebuild the
	// whole wiki on any commit, so these are reported, not regenerated.
	related := map[string][]string{
		"alpha":     {"beta"},
		"gamma":     {"alpha"},
		"unrelated": {"delta"},
	}
	dirty := map[string]bool{"beta": true}

	got := StaleAdjacent(related, dirty)
	if want := []string{"alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StaleAdjacent = %v, want %v (gamma is two hops away and must not be included)", got, want)
	}
}

func containsKey(keys []Key, want Key) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
