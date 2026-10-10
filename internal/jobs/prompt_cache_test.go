package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// These tests pin the prompt layout that prompt caching depends on. A cache is a
// prefix match, so nothing here is about what the model is told; it is about
// which bytes are identical from one call to the next, and where the material
// that is expensive to send sits relative to the material that changes.

// sourceMarker is text that appears only in the unit's source file, so its
// presence in a prompt means the source material was sent.
const sourceMarker = "func dispatchTheDistinctiveMarker() {}"

// generateRuleMarker and analyzeRuleMarker each appear in exactly one step's
// rules, so they show which step's rules a prompt carries.
const (
	generateRuleMarker = "NEVER write index.md"
	analyzeRuleMarker  = "Do not write files in this step"
)

func cacheTestUnit() mapper.Unit {
	return mapper.Unit{
		Key: "module:ripple", Kind: "module", Slug: "ripple", Title: "Ripple",
		Dir: "apps/ripple", Inputs: []string{"apps/ripple/main.go"}, Hash: "h1",
	}
}

func cacheTestRequest(t *testing.T) BuildRequest {
	t.Helper()
	req := testRequest(t, testMap(cacheTestUnit()), diff.ChangeSet{FullRebuild: true})
	req.Map.Summary = "# Repository map\n\nOne module: apps/ripple."
	if err := writeFile(req.SourceDir, "apps/ripple/main.go", "package main\n\n"+sourceMarker+"\n"); err != nil {
		t.Fatal(err)
	}
	return req
}

type cliCall struct {
	Args []string `json:"args"`
}

func (c cliCall) prompt() string { return c.Args[len(c.Args)-1] }

func (c cliCall) has(flag string) bool {
	for _, a := range c.Args {
		if a == flag {
			return true
		}
	}
	return false
}

func readCLICalls(t *testing.T, path string) []cliCall {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the CLI was never invoked: %v", err)
	}
	defer f.Close()

	var calls []cliCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var c cliCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

// TestCLIRunnerResumesWithoutBreakingOrRepeatingTheCachedContext drives the real
// pipeline and the real ClaudeRunner down to argv for a unit whose generate step
// fails validation twice, so it makes one analyze call and three generate calls.
func TestCLIRunnerResumesWithoutBreakingOrRepeatingTheCachedContext(t *testing.T) {
	calls := runCLIUnitWithRetries(t)
	if len(calls) != 4 {
		t.Fatalf("got %d CLI calls, want analyze plus three generate attempts", len(calls))
	}
	analyze, generates := calls[0], calls[1:]

	// The system prompt is the front of the cached prefix: resuming the analyze
	// session under different bytes re-reads the whole history uncached.
	system := flagValue(analyze.Args, "--append-system-prompt")
	if system == "" {
		t.Fatal("analyze carried no system prompt")
	}
	for i, g := range generates {
		if got := flagValue(g.Args, "--append-system-prompt"); got != system {
			t.Errorf("generate attempt %d has a different --append-system-prompt from analyze:\n got: %q\nwant: %q", i, got, system)
		}
	}
	// The step-specific rules moved to the user prompt; neither belongs in the
	// shared system prompt, or the two steps would no longer share it.
	for _, marker := range []string{generateRuleMarker, analyzeRuleMarker} {
		if strings.Contains(system, marker) {
			t.Errorf("system prompt still carries a step's rules (%q)", marker)
		}
	}
	if !strings.Contains(system, "UNTRUSTED DATA") || !strings.Contains(system, "# Repository map") {
		t.Errorf("system prompt lost the shared framing or the map:\n%s", system)
	}

	// The rules are not lost, only relocated.
	if !strings.Contains(analyze.prompt(), analyzeRuleMarker) {
		t.Error("analyze prompt lost its rules")
	}
	for i, g := range generates {
		if !strings.Contains(g.prompt(), generateRuleMarker) {
			t.Errorf("generate attempt %d lost its rules", i)
		}
	}

	// The source material is sent once, in analyze. Generate resumes a session
	// that already holds it.
	if !strings.Contains(analyze.prompt(), "## Source material") || !strings.Contains(analyze.prompt(), sourceMarker) {
		t.Errorf("analyze prompt carries no source material:\n%s", analyze.prompt())
	}
	for i, g := range generates {
		if strings.Contains(g.prompt(), "## Source material") || strings.Contains(g.prompt(), sourceMarker) {
			t.Errorf("generate attempt %d repeats the source material the resumed session already has:\n%s", i, g.prompt())
		}
		if !strings.Contains(g.prompt(), "earlier in this conversation") {
			t.Errorf("generate attempt %d does not say where the material is", i)
		}
	}

	// Every attempt, the first included, forks from the analyze session by its
	// deterministic id, so a retry starts from the analysis and not from the
	// transcript of the attempts that failed.
	sessionID := flagValue(analyze.Args, "--session-id")
	if !looksLikeUUID(sessionID) {
		t.Fatalf("analyze --session-id = %q", sessionID)
	}
	for i, g := range generates {
		if got := flagValue(g.Args, "--resume"); got != sessionID {
			t.Errorf("generate attempt %d resumes %q, want the analyze session %q", i, got, sessionID)
		}
		if !g.has("--fork-session") {
			t.Errorf("generate attempt %d does not fork the analyze session; it would accumulate the failed attempts", i)
		}
		if g.has("--session-id") {
			t.Errorf("generate attempt %d names a session id of its own", i)
		}
	}
	if analyze.has("--fork-session") {
		t.Error("analyze forks; it is the session the others fork from")
	}

	// The feedback that replaces the dropped transcript.
	if strings.Contains(generates[0].prompt(), "previous attempt failed validation") {
		t.Error("the first attempt claims a previous one failed")
	}
	for i, g := range generates[1:] {
		if !strings.Contains(g.prompt(), "previous attempt failed validation") {
			t.Errorf("retry %d carries no feedback", i+1)
		}
	}
}

// TestCLIRunnerStillGrantsTheScratchDirOnEveryForkedAttempt: --add-dir belongs
// to the invocation, not the session, so a fork does not inherit the grant. A
// retry that lost it would write into a directory it cannot, or nowhere.
func TestCLIRunnerStillGrantsTheScratchDirOnEveryForkedAttempt(t *testing.T) {
	calls := runCLIUnitWithRetries(t)
	for i, g := range calls[1:] {
		dir := flagValue(g.Args, "--add-dir")
		if dir == "" {
			t.Fatalf("generate attempt %d was granted no scratch directory", i)
		}
		if !strings.Contains(g.prompt(), dir) {
			t.Errorf("generate attempt %d does not name its scratch directory %q in the prompt", i, dir)
		}
	}
}

func runCLIUnitWithRetries(t *testing.T) []cliCall {
	t.Helper()

	argvLog := filepath.Join(t.TempDir(), "argv.jsonl")
	script, err := json.Marshal(map[string]any{
		"append_args_to": argvLog,
		// A type/directory mismatch: it fails validation on every attempt, which
		// is what makes the pipeline retry.
		"write_files": map[string]string{"entities/ripple.md": validPage("concept", "Ripple")},
		"envelope": map[string]any{
			"subtype": "success", "terminal_reason": "completed",
			"total_cost_usd": 0.01, "num_turns": 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &agent.ClaudeRunner{
		Binary: fakeClaudeBinary(t),
		Env:    append(os.Environ(), "KILN_FAKE_SCRIPT="+string(script)),
	}

	p := testPipeline(newMemStore(), runner)
	p.MaxRetries = 2
	if _, err := p.Build(context.Background(), cacheTestRequest(t)); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A full rebuild also plans the architecture unit; keep only the module's
	// calls, found by the session they open or resume.
	want := agent.SessionUUID("run-1-" + sanitize("module:ripple"))
	var mine []cliCall
	for _, c := range readCLICalls(t, argvLog) {
		if flagValue(c.Args, "--session-id") == want || flagValue(c.Args, "--resume") == want {
			mine = append(mine, c)
		}
	}
	return mine
}
