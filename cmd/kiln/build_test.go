package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

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

func TestSmallHelpers(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
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
		{"version"}, {"build"}, {"serve"}, {"worker"},
		{"admin", "migrate"}, {"admin", "doctor"}, {"admin", "rotate-key"},
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
