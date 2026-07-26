package jobs

import (
	"regexp"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
)

func TestCosmeticPath(t *testing.T) {
	cosmetic := []string{
		"assets/logo.PNG", "web/font.woff2", "vendor.tar.gz",
		"go.sum", "Cargo.lock", "package-lock.json", "docs/.gitignore",
	}
	for _, p := range cosmetic {
		if !cosmeticPath(p) {
			t.Errorf("cosmeticPath(%q) = false, want true", p)
		}
	}
	// Manifests and source are never cosmetic here -- the router exempts
	// manifests itself, and code obviously justifies a call.
	for _, p := range []string{"go.mod", "main.go", "docs/guide.md", "Makefile"} {
		if cosmeticPath(p) {
			t.Errorf("cosmeticPath(%q) = true, want false", p)
		}
	}
}

func TestRouterForRegistersSubPartitionsAndSkipsEmpty(t *testing.T) {
	rm := &repomap.RepoMap{
		Modules: []repomap.Module{
			{Slug: "root", Dir: "."},
			{Slug: "root-internal-auth", Dir: "internal/auth"},
			{Slug: "ghost", Dir: "ghost", Empty: true},
		},
		Docs: []repomap.DocFile{{Path: "README.md"}},
	}

	r := routerFor(rm)

	// Sub-partitions are why a large repo becomes readable pages: they must
	// be registered alongside the parent, and longest prefix must win.
	plan := r.Route(diff.ChangeSet{Changes: []diff.Change{
		{Path: "internal/auth/pg.go", Kind: diff.Modified},
	}})
	if len(plan.Dirty) == 0 {
		t.Fatal("changed path routed to nothing")
	}
	found := false
	for _, k := range plan.Dirty {
		if k == diff.ModuleKey("root-internal-auth") {
			found = true
		}
		if k == diff.ModuleKey("ghost") {
			t.Error("empty module registered in the router")
		}
	}
	if !found {
		t.Errorf("internal/auth change routed to %v, want the sub-partition key", plan.Dirty)
	}
}

func TestSectionsByParent(t *testing.T) {
	wm := &mapper.WorkspaceMap{Units: []mapper.Unit{
		{Key: "doc:guide.md", Kind: "doc"},
		{Key: "doc:guide.md#one", Kind: "doc-section", Meta: map[string]any{"parent": "doc:guide.md"}},
		{Key: "doc:guide.md#two", Kind: "doc-section", Meta: map[string]any{"parent": "doc:guide.md"}},
		{Key: "doc:orphan#x", Kind: "doc-section"}, // no parent: skipped, not crashed
	}}

	got := sectionsByParent(wm)
	if len(got[diff.Key("doc:guide.md")]) != 2 {
		t.Errorf("sections for guide = %v, want 2", got[diff.Key("doc:guide.md")])
	}
	if len(got) != 1 {
		t.Errorf("parents = %d, want 1 (orphan skipped)", len(got))
	}
}

func TestNewRunIDShape(t *testing.T) {
	re := regexp.MustCompile(`^run-[0-9a-f]{16}$`)
	a, b := NewRunID(), NewRunID()
	if !re.MatchString(a) {
		t.Errorf("run id %q does not match run-<16 hex>", a)
	}
	if a == b {
		t.Error("two run ids collided")
	}
}

func TestGitRefHelpers(t *testing.T) {
	if got := shortRef("0123456789"); got != "0123456" {
		t.Errorf("shortRef = %q", got)
	}
	if got := gitRef(&repomap.RepoMap{}); got != "" {
		t.Errorf("gitRef without git meta = %q, want empty", got)
	}
	if got := gitRef(&repomap.RepoMap{Git: &repomap.GitMeta{HeadSHA: "abcdef012345"}}); got != "abcdef0" {
		t.Errorf("gitRef = %q", got)
	}
}
