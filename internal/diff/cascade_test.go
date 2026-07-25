package diff

import (
	"reflect"
	"testing"
)

// threeModules is the case reference counting exists for: a concept page
// derived from all three, plus one page unique to each.
func threeModules() []SourceRecord {
	return []SourceRecord{
		{
			Key:          ModuleKey("alpha"),
			FilesWritten: []string{"entities/alpha.md", "concepts/shared-pattern.md"},
			BlobKeys:     []string{"blob-alpha"},
		},
		{
			Key:          ModuleKey("beta"),
			FilesWritten: []string{"entities/beta.md", "concepts/shared-pattern.md"},
			BlobKeys:     []string{"blob-beta"},
		},
		{
			Key:          ModuleKey("gamma"),
			FilesWritten: []string{"entities/gamma.md", "concepts/shared-pattern.md"},
		},
	}
}

func TestPlanCascadeKeepsSharedPages(t *testing.T) {
	got := PlanCascade(threeModules(), []Key{ModuleKey("alpha")})

	// The page unique to alpha goes; the page all three produced stays.
	if want := []string{"entities/alpha.md"}; !reflect.DeepEqual(got.DeletePages, want) {
		t.Errorf("DeletePages = %v, want %v", got.DeletePages, want)
	}

	// Crucially it is regenerated, not merely amended: its prose still
	// describes a module that no longer exists.
	if want := []string{"concepts/shared-pattern.md"}; !reflect.DeepEqual(got.RegeneratePages, want) {
		t.Errorf("RegeneratePages = %v, want %v", got.RegeneratePages, want)
	}
}

func TestPlanCascadeDeletesWhenLastClaimGoes(t *testing.T) {
	all := threeModules()
	got := PlanCascade(all, []Key{ModuleKey("alpha"), ModuleKey("beta"), ModuleKey("gamma")})

	want := []string{
		"concepts/shared-pattern.md",
		"entities/alpha.md",
		"entities/beta.md",
		"entities/gamma.md",
	}
	if !reflect.DeepEqual(got.DeletePages, want) {
		t.Errorf("DeletePages = %v, want %v", got.DeletePages, want)
	}
	if len(got.RegeneratePages) != 0 {
		t.Errorf("RegeneratePages = %v, want none", got.RegeneratePages)
	}
}

func TestPlanCascadeBlobReferenceCounting(t *testing.T) {
	all := []SourceRecord{
		{Key: DocKey("a.pdf"), FilesWritten: []string{"sources/a.md"}, BlobKeys: []string{"shared-blob", "blob-a"}},
		{Key: DocKey("b.pdf"), FilesWritten: []string{"sources/b.md"}, BlobKeys: []string{"shared-blob"}},
	}

	got := PlanCascade(all, []Key{DocKey("a.pdf")})

	// A blob another source still references must survive, or deleting one
	// document would break the other's evidence links.
	if want := []string{"blob-a"}; !reflect.DeepEqual(got.DeleteBlobs, want) {
		t.Errorf("DeleteBlobs = %v, want %v", got.DeleteBlobs, want)
	}
}

func TestPlanCascadeDropsSourceRows(t *testing.T) {
	got := PlanCascade(threeModules(), []Key{ModuleKey("beta"), ModuleKey("alpha")})

	want := []Key{ModuleKey("alpha"), ModuleKey("beta")}
	if !reflect.DeepEqual(got.DropSources, want) {
		t.Errorf("DropSources = %v, want %v (sorted)", got.DropSources, want)
	}
}

func TestPlanCascadeEmpty(t *testing.T) {
	if got := PlanCascade(threeModules(), nil); !got.Empty() {
		t.Errorf("removing nothing produced a cascade: %+v", got)
	}
}

func TestPlanCascadeUnknownKeyIsHarmless(t *testing.T) {
	// A key that is not on record must not delete anything.
	got := PlanCascade(threeModules(), []Key{ModuleKey("never-existed")})
	if len(got.DeletePages) != 0 || len(got.RegeneratePages) != 0 {
		t.Errorf("unknown key affected pages: %+v", got)
	}
}

func TestPlanCascadePageCount(t *testing.T) {
	// This is the number a deletion review shows before the user confirms, so
	// it must count only what actually disappears.
	got := PlanCascade(threeModules(), []Key{ModuleKey("alpha")})
	if got.PageCount() != 1 {
		t.Errorf("PageCount() = %d, want 1 (the shared page survives)", got.PageCount())
	}
}

func TestPlanCascadeIsDeterministic(t *testing.T) {
	all := threeModules()
	first := PlanCascade(all, []Key{ModuleKey("alpha"), ModuleKey("beta")})

	for range 20 {
		if got := PlanCascade(all, []Key{ModuleKey("beta"), ModuleKey("alpha")}); !reflect.DeepEqual(got, first) {
			t.Fatal("PlanCascade output varies with input order or map iteration")
		}
	}
}
