package jobs

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// The CLI runner is exercised here through the real *agent.ClaudeRunner and a
// real subprocess, not the in-process fakes the rest of this package uses.
//
// That distinction is the whole point. Every CLI-runner bug found so far lived
// in what the pipeline hands the runner rather than in the runner itself: a
// session id the CLI rejects because it is not a UUID, and a working directory
// taken from the run instead of the unit. Both passed every test in this
// package, because the fakes accept any Request; and both passed every test in
// internal/agent, because those construct the Request themselves. Only the two
// halves together can catch that class, so this test drives the pipeline down
// to argv.

var (
	buildCLIOnce sync.Once
	cliPath      string
	cliBuildErr  error
)

// fakeClaudeBinary compiles the fixture that stands in for the claude CLI. A
// real binary rather than a mock so the exec path -- argv, working directory,
// environment, exit code, envelope on stdout -- is genuinely exercised.
func fakeClaudeBinary(t *testing.T) string {
	t.Helper()

	buildCLIOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kiln-fakeclaude-jobs-")
		if err != nil {
			cliBuildErr = err
			return
		}
		out := filepath.Join(dir, "claude")
		cmd := exec.Command("go", "build", "-o", out, "../agent/testdata/fakeclaude")
		if combined, err := cmd.CombinedOutput(); err != nil {
			cliBuildErr = err
			t.Logf("build output: %s", combined)
			return
		}
		cliPath = out
	})

	if cliBuildErr != nil {
		t.Fatalf("build fakeclaude: %v", cliBuildErr)
	}
	return cliPath
}

// cliRunner returns a real ClaudeRunner driving the fixture, scripted to write
// the given files into the scratch directory it is granted.
func cliRunner(t *testing.T, files map[string]string, argvLog string) *agent.ClaudeRunner {
	t.Helper()

	script, err := json.Marshal(map[string]any{
		"write_files":    files,
		"record_args_to": argvLog,
		"envelope": map[string]any{
			"subtype":         "success",
			"terminal_reason": "completed",
			"total_cost_usd":  0.01,
			"num_turns":       1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return &agent.ClaudeRunner{
		Binary: fakeClaudeBinary(t),
		// The fixture is driven by this variable, so the child environment is
		// inherited rather than minimised here. Production construction goes
		// through agent.New, which sets MinimalChildEnv; that it does so is
		// asserted in internal/agent.
		Env: append(os.Environ(), "KILN_FAKE_SCRIPT="+string(script)),
	}
}

// A repository bench, end to end: the pipeline plans, invokes the CLI, collects
// what it wrote from the scratch directory, validates, and imports.
func TestCLIRunnerBuildsPagesThroughTheWholePipeline(t *testing.T) {
	store := newMemStore()
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	runner := cliRunner(t, map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}, argvLog)

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status == StatusFailed {
		t.Fatalf("build failed: %s (violations: %v)", res.Summary.Err, res.Violations)
	}

	imp := store.lastImport()
	if imp == nil {
		t.Fatal("the CLI runner produced no import: nothing was collected from the scratch directory")
	}
	var found bool
	for _, pg := range imp.UpsertPages {
		if pg.Path == "entities/ripple.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("the page the CLI wrote never reached the import: %+v", imp.UpsertPages)
	}
}

// The argv the pipeline ultimately produces. Asserted here rather than in
// internal/agent because the values under test originate in the pipeline: it
// decides the session id and the working directory, and a unit test that
// constructs its own Request cannot catch either being wrong.
func TestCLIRunnerReceivesAUsableInvocation(t *testing.T) {
	store := newMemStore()
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	runner := cliRunner(t, map[string]string{
		"entities/ripple.md": validPage("entity", "Ripple"),
	}, argvLog)

	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var rec struct {
		Args []string `json:"args"`
		Cwd  string   `json:"cwd"`
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the CLI was never invoked: %v", err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}

	// The session id must be a UUID. It is not: the pipeline builds a readable
	// key ("run-1-module_ripple"), and passing that straight through made the
	// CLI exit non-zero on every unit of every build.
	if id := flagValue(rec.Args, "--session-id"); id != "" {
		if !looksLikeUUID(id) {
			t.Errorf("--session-id = %q, which the CLI rejects as not a UUID", id)
		}
	}
	if resume := flagValue(rec.Args, "--resume"); resume != "" && !looksLikeUUID(resume) {
		t.Errorf("--resume = %q, which the CLI rejects as not a UUID", resume)
	}

	// And the working directory must exist and be the unit's sources. Empty is
	// what a documents-only bench used to produce, and the runner refuses it.
	if rec.Cwd == "" {
		t.Fatal("the CLI was given no working directory")
	}
	if fi, err := os.Stat(rec.Cwd); err != nil || !fi.IsDir() {
		t.Errorf("working directory %q is not a directory: %v", rec.Cwd, err)
	}
}

// A bench of only uploaded documents has no checkout, so the run carries no
// source directory and each unit's own staged root is the only one there is.
// This is the shape that failed with "agent: WorkDir is required" before the
// pipeline resolved the working directory per unit -- and it failed for every
// unit, having made no model call at all.
func TestCLIRunnerBuildsADocumentsOnlyBench(t *testing.T) {
	store := newMemStore()
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	runner := cliRunner(t, map[string]string{
		"sources/slides.md": validPage("source", "Slides"),
	}, argvLog)

	staging := t.TempDir()
	p := testPipeline(store, runner)
	m := &mapper.WorkspaceMap{Kind: "upload", Units: []mapper.Unit{{
		Key: "doc:upload:slides.pdf", Slug: "slides", Hash: "h",
		Meta: map[string]any{mapper.MetaRoot: staging},
	}}}

	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.SourceDir = ""
	req.Router = diff.Router{DocPaths: map[string]diff.Key{
		"upload:slides.pdf": "doc:upload:slides.pdf",
	}}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status == StatusFailed {
		t.Fatalf("documents-only build failed: %s", res.Summary.Err)
	}

	imp := store.lastImport()
	if imp == nil || len(imp.UpsertPages) == 0 {
		t.Fatal("the document produced no pages")
	}

	var rec struct {
		Cwd string `json:"cwd"`
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the CLI was never invoked: %v", err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	// Compared through EvalSymlinks: the child reports its working directory
	// via getwd, which resolves symlinks, and on macOS every t.TempDir() sits
	// under /var -> /private/var. Comparing the raw strings would fail on the
	// platform this is most often run on, for no reason anyone cares about.
	if !sameDir(t, rec.Cwd, staging) {
		t.Errorf("cwd = %q, want the document's staged root %q", rec.Cwd, staging)
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// The end-to-end proof: a real claude, a real model call, real prose imported
// as a page. Everything above this uses a fixture, which establishes that the
// pipeline drives the CLI correctly but not that the CLI then produces a wiki.
//
// Guarded by KILN_TEST_CLAUDE and skipped without it, matching how the database
// integration tests key off KILN_TEST_DATABASE_URL: the suite stays fast,
// offline and free by default, and the expensive check is one variable away
// rather than a procedure someone has to remember. It costs a few cents.
//
//	claude auth login                       # or set ANTHROPIC_API_KEY
//	KILN_TEST_CLAUDE=1 go test ./internal/jobs/ -run TestCLIRunnerAgainstRealClaude -v
func TestCLIRunnerAgainstRealClaude(t *testing.T) {
	if os.Getenv("KILN_TEST_CLAUDE") == "" {
		t.Skip("set KILN_TEST_CLAUDE=1 to run one real claude invocation (costs a few cents)")
	}

	binary := os.Getenv("KILN_AGENT_BINARY")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	baseURL := os.Getenv("KILN_AGENT_BASE_URL")

	// Checked before spending anything, so an unauthenticated CLI fails with
	// the reason rather than with a run that dies on its first unit.
	h, err := agent.CheckCLI(context.Background(), binary, apiKey, baseURL)
	if err != nil {
		t.Fatalf("the claude CLI is unusable: %v", err)
	}
	t.Logf("claude: %s", h.Summary())
	if !h.Usable() && h.AuthErr == nil {
		t.Fatalf("the claude CLI is not authenticated: run `claude auth login`")
	}

	// One small real source, so the agent has something specific to write about
	// and the call stays cheap.
	sources := t.TempDir()
	const doc = "ledger.md"
	body := "# The Budget Ledger\n\n" +
		"kiln reserves budget before each model call and settles it afterwards.\n" +
		"Reserving first is what makes the run budget a hard ceiling at any\n" +
		"concurrency: without it, several units in flight could each pass a\n" +
		"check and then collectively overspend. A unit that cannot reserve has\n" +
		"not spent anything, so it is recorded as deferred rather than failed.\n"
	if err := os.WriteFile(filepath.Join(sources, doc), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := agent.NewClaudeRunner(binary)
	runner.Env = agent.MinimalChildEnv(apiKey, baseURL)

	store := newMemStore()
	p := testPipeline(store, runner)
	// Budgets above the per-call defaults: a document's text rides in the
	// prompt, and the stock analyze ceiling is sized for a repository unit.
	p.Budget = Budget{AnalyzeUSD: 1.0, PageUSD: 2.0, RunUSD: 5.0, MaxPages: 4}

	m := &mapper.WorkspaceMap{Kind: "upload", Units: []mapper.Unit{{
		Key: "doc:upload:" + doc, Slug: "budget-ledger", Hash: "h",
		Title: "The Budget Ledger", Inputs: []string{doc},
		Meta: map[string]any{mapper.MetaRoot: sources},
	}}}

	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.SourceDir = ""
	req.Router = diff.Router{DocPaths: map[string]diff.Key{
		"upload:" + doc: diff.Key("doc:upload:" + doc),
	}}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status == StatusFailed {
		t.Fatalf("the real CLI runner failed: %s (violations: %v)", res.Summary.Err, res.Violations)
	}

	imp := store.lastImport()
	if imp == nil || len(imp.UpsertPages) == 0 {
		t.Fatal("the real CLI runner produced no pages")
	}
	for _, pg := range imp.UpsertPages {
		if len(strings.TrimSpace(pg.Body)) < 100 {
			t.Errorf("page %s came back essentially empty: %q", pg.Path, pg.Body)
		}
		t.Logf("wrote %s (%q, %d bytes)", pg.Path, pg.Meta.Title, len(pg.Body))
	}
	t.Logf("cost $%.4f across %d unit(s)", res.Summary.CostUSD, len(res.Summary.Items))
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// looksLikeUUID checks the shape the CLI validates: 8-4-4-4-12 hex.
func looksLikeUUID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(parts[i]) != want {
			return false
		}
		for _, c := range parts[i] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
