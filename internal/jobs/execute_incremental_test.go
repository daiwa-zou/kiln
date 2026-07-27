package jobs

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	"github.com/daiwa-zou/kiln/internal/diff"
)

// incrementalFixture builds a git repo with two Go modules, commits, touches
// only the second module, commits again, and returns the dir plus both SHAs.
func incrementalFixture(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeTree(t, dir, map[string]string{
		"alpha/go.mod":  "module example.com/alpha\n\ngo 1.25\n",
		"alpha/main.go": "package main\n\nfunc main() {}\n",
		"beta/go.mod":   "module example.com/beta\n\ngo 1.25\n",
		"beta/lib.go":   "package beta\n\nfunc Beta() int { return 1 }\n",
	})
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-q", "-m", "first")
	first = gitconn.HeadRef(context.Background(), dir)

	writeTree(t, dir, map[string]string{
		"beta/lib.go": "package beta\n\nfunc Beta() int { return 2 }\n",
	})
	run("add", "-A")
	run("commit", "-q", "-m", "second")
	second = gitconn.HeadRef(context.Background(), dir)
	return dir, first, second
}

// TestExecuteIncrementalRoutesOnlyTouchedUnits is M5's verification case: a
// commit touching one module plans exactly that module, not the workspace.
func TestExecuteIncrementalRoutesOnlyTouchedUnits(t *testing.T) {
	dir, first, _ := incrementalFixture(t)
	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "run-inc", WorkspaceID: "ws-1", Trigger: "webhook",
		Source:  SourceSpec{Path: dir, Slug: "bench"},
		BaseRef: first,
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var sawBeta bool
	for _, k := range res.Planned {
		if strings.Contains(string(k), "alpha") {
			t.Errorf("untouched module planned: %v", res.Planned)
		}
		if strings.Contains(string(k), "beta") {
			sawBeta = true
		}
		// No manifest changed, so the architecture synthesis stays quiet.
		if k == diff.ArchOverview {
			t.Errorf("arch overview planned without a manifest change: %v", res.Planned)
		}
	}
	if !sawBeta {
		t.Fatalf("touched module missing from plan: %v", res.Planned)
	}
}

func TestExecuteUnusableRangeFallsBackToFull(t *testing.T) {
	dir, _, _ := incrementalFixture(t)
	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "run-fb", WorkspaceID: "ws-1", Trigger: "webhook",
		Source:  SourceSpec{Path: dir, Slug: "bench"},
		BaseRef: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", // force-push shape
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Both modules plus the architecture synthesis: full-rebuild routing.
	var alpha, beta, arch bool
	for _, k := range res.Planned {
		alpha = alpha || strings.Contains(string(k), "alpha")
		beta = beta || strings.Contains(string(k), "beta")
		arch = arch || k == diff.ArchOverview
	}
	if !alpha || !beta || !arch {
		t.Errorf("fallback plan incomplete: %v", res.Planned)
	}
}

func TestExecuteManifestChangeRoutesArch(t *testing.T) {
	dir, _, second := incrementalFixture(t)
	// A third commit that edits a manifest: arch must join the plan.
	writeTree(t, dir, map[string]string{
		"beta/go.mod": "module example.com/beta\n\ngo 1.25\n\nrequire example.com/alpha v0.0.0\n",
	})
	cmd := exec.Command("git", "-C", dir, "add", "-A")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-q", "-m", "third")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())
	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "run-arch", WorkspaceID: "ws-1", Trigger: "webhook",
		Source:  SourceSpec{Path: dir, Slug: "bench"},
		BaseRef: second,
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var arch bool
	for _, k := range res.Planned {
		arch = arch || k == diff.ArchOverview
	}
	if !arch {
		t.Errorf("manifest change did not route the architecture synthesis: %v", res.Planned)
	}
}
