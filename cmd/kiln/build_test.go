package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
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

func TestReportDryRun(t *testing.T) {
	var buf bytes.Buffer
	err := report(&buf, &jobs.BuildResult{
		Planned:      []diff.Key{diff.ModuleKey("ripple"), diff.ArchOverview},
		EstimatedUSD: 0.24,
		Deferred:     3,
	}, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"plan: 2 unit(s), estimated $0.24",
		"module:ripple",
		"3 more unit(s) exceed the per-run page cap",
		"no model calls were made (--dry-run)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, out)
		}
	}
}

func TestReportNoChangesIsExplicit(t *testing.T) {
	var buf bytes.Buffer
	res := &jobs.BuildResult{Summary: jobs.RunSummary{Status: jobs.StatusNoChanges}}
	if err := report(&buf, res, false, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The design working must read as success, not as an empty run.
	if !strings.Contains(buf.String(), "nothing changed; no model calls, no cost") {
		t.Errorf("no-changes report:\n%s", buf.String())
	}
}

func TestReportFailuresAndViolations(t *testing.T) {
	var buf bytes.Buffer
	res := &jobs.BuildResult{
		Summary: jobs.RunSummary{
			Status: jobs.StatusPartial, CostUSD: 1.5, Created: 2, Updated: 1,
			Items: []jobs.ItemSummary{
				{Key: diff.ModuleKey("ok"), Status: jobs.StatusSucceeded},
				{Key: diff.ModuleKey("bad"), Status: jobs.StatusFailed, Err: "boom"},
			},
		},
	}
	if err := report(&buf, res, false, time.Second); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "FAILED module:bad: boom") {
		t.Errorf("failed item not reported:\n%s", out)
	}
	if !strings.Contains(out, "pages: 2 created, 1 updated, 0 removed") {
		t.Errorf("page summary wrong:\n%s", out)
	}
}

func TestNewRunIDShape(t *testing.T) {
	re := regexp.MustCompile(`^run-[0-9a-f]{16}$`)
	a, b := newRunID(), newRunID()
	if !re.MatchString(a) {
		t.Errorf("run id %q does not match run-<16 hex>", a)
	}
	if a == b {
		t.Error("two run ids collided")
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
	}
	if got := short("0123456789"); got != "0123456" {
		t.Errorf("short = %q", got)
	}
	if got := gitRef(&repomap.RepoMap{}); got != "" {
		t.Errorf("gitRef without git meta = %q, want empty", got)
	}
	if got := gitRef(&repomap.RepoMap{Git: &repomap.GitMeta{HeadSHA: "abcdef012345"}}); got != "abcdef0" {
		t.Errorf("gitRef = %q", got)
	}
	if got := splitScopes(" read, write ,"); len(got) != 2 || got[0] != "read" || got[1] != "write" {
		t.Errorf("splitScopes = %v", got)
	}
}

func TestCommandTreeShape(t *testing.T) {
	root := newRootCmd()

	// The CLI contract: these commands exist and the dangerous flags are
	// required. Losing one in a refactor should fail a test, not a user.
	for _, path := range [][]string{
		{"version"}, {"build"}, {"serve"},
		{"admin", "migrate"}, {"admin", "doctor"},
		{"admin", "token", "create"}, {"admin", "token", "revoke"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("command %v not found: %v", path, err)
		}
	}

	create, _, _ := root.Find([]string{"admin", "token", "create"})
	if ann := create.Flags().Lookup("login"); ann == nil {
		t.Fatal("token create has no --login flag")
	} else if req, ok := ann.Annotations["cobra_annotation_bash_completion_one_required_flag"]; !ok || len(req) == 0 || req[0] != "true" {
		t.Error("token create --login is not marked required")
	}

	// Persistent flags reach subcommands.
	build, _, _ := root.Find([]string{"build"})
	if build.InheritedFlags().Lookup("config") == nil {
		t.Error("--config not inherited by build")
	}
}

func TestVersionCommandPrints(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(buf.String()) == "" {
		t.Error("version printed nothing")
	}
}
