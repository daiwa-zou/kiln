package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	buildFakeOnce sync.Once
	fakePath      string
	fakeErr       error
)

// fakeClaude builds the stand-in binary once per test run. It is a real
// executable so the exec path itself is under test.
func fakeClaude(t *testing.T) string {
	t.Helper()

	buildFakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kiln-fakeclaude-")
		if err != nil {
			fakeErr = err
			return
		}
		out := filepath.Join(dir, "claude")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/fakeclaude")
		if combined, err := cmd.CombinedOutput(); err != nil {
			fakeErr = err
			t.Logf("build output: %s", combined)
			return
		}
		fakePath = out
	})

	if fakeErr != nil {
		t.Fatalf("build fakeclaude: %v", fakeErr)
	}
	return fakePath
}

func setScript(t *testing.T, s map[string]any) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_FAKE_SCRIPT", string(b))
}

func analyzeReq(dir string) Request {
	return Request{
		Step: StepAnalyze, WorkDir: dir, SessionID: "s-1",
		Model: "sonnet", BudgetUSD: 0.40, Prompt: "analyze this",
		Timeout: 30 * time.Second,
	}
}

func TestBuildArgsAnalyze(t *testing.T) {
	args := BuildArgs(Request{
		Step: StepAnalyze, WorkDir: "/src", SessionID: "abc",
		Model: "sonnet", BudgetUSD: 0.4, JSONSchema: `{"type":"object"}`,
		SystemPrompt: "be careful", Prompt: "go",
	})

	joined := strings.Join(args, " ")

	// Isolation from source-supplied config is not optional: ingested repos
	// carry .claude directories that are untrusted input on a shared platform.
	for _, want := range []string{"--setting-sources", "--strict-mcp-config", "--no-session-persistence"} {
		if !containsArg(args, want) {
			t.Errorf("missing %s in: %s", want, joined)
		}
	}

	if got := flagVal(args, "--tools"); got != "Read,Grep,Glob" {
		t.Errorf("--tools = %q, want read-only set", got)
	}
	if got := flagVal(args, "--permission-mode"); got != "dontAsk" {
		t.Errorf("--permission-mode = %q, want dontAsk", got)
	}
	if got := flagVal(args, "--max-budget-usd"); got != "0.4" {
		t.Errorf("--max-budget-usd = %q, want 0.4", got)
	}
	// The CLI rejects a session id that is not a UUID, so the key is derived
	// rather than passed through.
	if got := flagVal(args, "--session-id"); got != SessionUUID("abc") {
		t.Errorf("--session-id = %q, want %q", got, SessionUUID("abc"))
	}
	if containsArg(args, "--add-dir") {
		t.Error("analyze must not be granted a writable directory")
	}
	if args[len(args)-1] != "go" {
		t.Errorf("prompt must be the final argument, got %q", args[len(args)-1])
	}
}

func TestBuildArgsGenerate(t *testing.T) {
	args := BuildArgs(Request{
		Step: StepGenerate, WorkDir: "/src", ScratchDir: "/scratch",
		SessionID: "abc", Model: "opus", FallbackModel: "sonnet",
		BudgetUSD: 1.5, Prompt: "write",
	})

	if got := flagVal(args, "--tools"); got != "Read,Grep,Glob,Write,Edit" {
		t.Errorf("--tools = %q", got)
	}

	// Bash is the single biggest reduction in attack surface available here.
	denied := flagVal(args, "--disallowedTools")
	for _, want := range []string{"Bash", "WebSearch", "WebFetch", "Task"} {
		if !strings.Contains(denied, want) {
			t.Errorf("--disallowedTools = %q, want it to include %s", denied, want)
		}
	}

	if got := flagVal(args, "--add-dir"); got != "/scratch" {
		t.Errorf("--add-dir = %q, want the scratch dir", got)
	}
	if got := flagVal(args, "--permission-mode"); got != "acceptEdits" {
		t.Errorf("--permission-mode = %q", got)
	}
	// Resuming keeps the source context prompt-cached across the two steps, so
	// it must derive the same id the analyze step opened.
	if got := flagVal(args, "--resume"); got != SessionUUID("abc") {
		t.Errorf("--resume = %q, want %q", got, SessionUUID("abc"))
	}
	if containsArg(args, "--session-id") {
		t.Error("generate should resume, not open a new session")
	}
}

// The CLI validates --session-id as a UUID and exits non-zero on anything
// else, which turned every unit of a CLI-runner build into "analyze step
// failed: exit status 1". The pipeline's readable key ("run-abc-doc-README-md")
// is not one, so it is derived here.
func TestSessionUUID(t *testing.T) {
	const key = "run-bff86c822909b88f-doc-README-md"

	got := SessionUUID(key)
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("SessionUUID(%q) = %q, which is not a UUID: %v", key, got, err)
	}

	// Determinism is load-bearing: the generate step resumes by the same id.
	if again := SessionUUID(key); again != got {
		t.Errorf("SessionUUID is not deterministic: %q then %q", got, again)
	}
	// Two units of one run must not collide onto the same session, or the
	// second would resume the first's context.
	if other := SessionUUID(key + "-2"); other == got {
		t.Error("distinct keys derived the same session id")
	}
}

func TestBuildArgsNeverUsesBypassPermissions(t *testing.T) {
	for _, step := range []Step{StepAnalyze, StepGenerate} {
		args := BuildArgs(Request{Step: step, WorkDir: "/src", ScratchDir: "/scratch", Prompt: "x"})
		if strings.Contains(strings.Join(args, " "), "bypassPermissions") {
			t.Errorf("%s step used bypassPermissions", step)
		}
		if containsArg(args, "--dangerously-skip-permissions") {
			t.Errorf("%s step skipped permission checks", step)
		}
	}
}

func TestBuildArgsOmitsUnsetOptionals(t *testing.T) {
	args := BuildArgs(Request{Step: StepAnalyze, WorkDir: "/src", Prompt: "x"})

	for _, absent := range []string{"--model", "--max-budget-usd", "--append-system-prompt", "--json-schema", "--session-id"} {
		if containsArg(args, absent) {
			t.Errorf("%s should be omitted when unset: %v", absent, args)
		}
	}
}

func TestRunParsesEnvelope(t *testing.T) {
	setScript(t, map[string]any{})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	res, err := r.Run(context.Background(), analyzeReq(t.TempDir()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !res.Succeeded() {
		t.Errorf("Succeeded() = false for a clean envelope: %+v", res)
	}
	if res.TotalCostUSD == 0 {
		t.Error("cost was not parsed")
	}
	if res.Usage.Total() == 0 {
		t.Error("usage was not parsed")
	}
	// The envelope reports the session the CLI actually ran, which is the
	// derived UUID rather than the caller's key.
	if want := SessionUUID("s-1"); res.SessionID != want {
		t.Errorf("SessionID = %q, want %q", res.SessionID, want)
	}
	if err := res.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

func TestRunSetsWorkingDirectory(t *testing.T) {
	// There is no --cwd flag, so the working directory is how the agent is
	// pointed at the sources. If this regressed it would silently analyze the
	// wrong tree.
	dir := t.TempDir()
	record := filepath.Join(t.TempDir(), "argv.json")
	setScript(t, map[string]any{"record_args_to": record})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	if _, err := r.Run(context.Background(), analyzeReq(dir)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got struct {
		Args []string `json:"args"`
		Cwd  string   `json:"cwd"`
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	// macOS temp dirs are symlinked via /private, so compare resolved paths.
	wantCwd, _ := filepath.EvalSymlinks(dir)
	gotCwd, _ := filepath.EvalSymlinks(got.Cwd)
	if gotCwd != wantCwd {
		t.Errorf("cwd = %q, want %q", gotCwd, wantCwd)
	}
}

func TestRunChildEnvExcludesSecrets(t *testing.T) {
	// The child processes untrusted source content; a leaked KILN_MASTER_KEY in
	// its environment would hand the platform's secrets to a prompt injection.
	t.Setenv("KILN_MASTER_KEY", "leak-me")
	t.Setenv("KILN_SESSION_SECRET", "leak-me-too")

	record := filepath.Join(t.TempDir(), "argv.json")
	script, err := json.Marshal(map[string]any{"record_args_to": record})
	if err != nil {
		t.Fatal(err)
	}

	env := MinimalChildEnv("sk-test", "")
	// The fake needs its script; the production env never carries it.
	env = append(env, "KILN_FAKE_SCRIPT="+string(script))

	r := &ClaudeRunner{Binary: fakeClaude(t), Env: env}
	if _, err := r.Run(context.Background(), analyzeReq(t.TempDir())); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got struct {
		Env []string `json:"env"`
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	sawAPIKey := false
	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "KILN_MASTER_KEY=") || strings.HasPrefix(kv, "KILN_SESSION_SECRET=") {
			t.Errorf("secret leaked into the child environment: %s", kv)
		}
		if kv == "ANTHROPIC_API_KEY=sk-test" {
			sawAPIKey = true
		}
	}
	if !sawAPIKey {
		t.Error("ANTHROPIC_API_KEY did not reach the child")
	}
}

// agent.base_url has always redirected the API runner. Passing it here too is
// what stops one configuration key meaning two different things depending on
// which runner is selected -- a key valid only at a gateway would otherwise
// fail to authenticate through the CLI with nothing to explain why.
func TestMinimalChildEnvPassesBaseURL(t *testing.T) {
	const url = "https://gateway.example.com"

	var saw bool
	for _, kv := range MinimalChildEnv("sk-test", url) {
		if kv == "ANTHROPIC_BASE_URL="+url {
			saw = true
		}
	}
	if !saw {
		t.Errorf("ANTHROPIC_BASE_URL=%s did not reach the child environment", url)
	}
}

func TestMinimalChildEnvOmitsUnsetKey(t *testing.T) {
	for _, kv := range MinimalChildEnv("", "") {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("empty key should be omitted, got %s", kv)
		}
		// An unset base URL must be absent rather than empty: the CLI reads an
		// empty ANTHROPIC_BASE_URL as a value, not as "use the default".
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			t.Errorf("empty base URL should be omitted, got %s", kv)
		}
		if strings.HasPrefix(kv, "KILN_") {
			t.Errorf("KILN_* variable in minimal env: %s", kv)
		}
	}
}

func TestRunErrorEnvelope(t *testing.T) {
	setScript(t, map[string]any{
		"envelope": map[string]any{
			"is_error": true, "subtype": "error_during_execution",
			"result": "the model refused", "terminal_reason": "error",
		},
	})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	res, err := r.Run(context.Background(), analyzeReq(t.TempDir()))
	if err != nil {
		t.Fatalf("Run returned a transport error for a valid error envelope: %v", err)
	}

	if res.Succeeded() {
		t.Error("Succeeded() = true for an error envelope")
	}
	if res.Err() == nil {
		t.Error("Err() = nil for an error envelope")
	}
	if !strings.Contains(res.Err().Error(), "the model refused") {
		t.Errorf("Err() should surface the reason, got %v", res.Err())
	}
}

func TestRunAPIErrorStatusIsFailure(t *testing.T) {
	// A 529 arrives inside an otherwise well-formed envelope, so checking the
	// exit code alone would treat an overload as success.
	setScript(t, map[string]any{
		"envelope": map[string]any{"api_error_status": 529},
	})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	res, err := r.Run(context.Background(), analyzeReq(t.TempDir()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded() {
		t.Error("an envelope carrying api_error_status was treated as success")
	}
}

func TestRunNonZeroExit(t *testing.T) {
	setScript(t, map[string]any{"exit_code": 1, "stdout": "not json", "stderr": "boom"})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	if _, err := r.Run(context.Background(), analyzeReq(t.TempDir())); err == nil {
		t.Fatal("Run succeeded despite a non-zero exit and unparseable output")
	}
}

func TestRunTimeout(t *testing.T) {
	// The installed CLI has no --max-turns, so this timeout is the only hard
	// stop on a runaway session.
	setScript(t, map[string]any{"sleep_ms": 2000})

	req := analyzeReq(t.TempDir())
	req.Timeout = 150 * time.Millisecond

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	_, err := r.Run(context.Background(), req)
	if err == nil {
		t.Fatal("Run succeeded past its timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should name the timeout, got: %v", err)
	}
}

func TestRunWritesToScratchDir(t *testing.T) {
	scratch := t.TempDir()
	setScript(t, map[string]any{
		"write_files": map[string]string{"concepts/x.md": "---\ntype: concept\n---\n# X\n"},
	})

	r := &ClaudeRunner{Binary: fakeClaude(t)}
	req := Request{
		Step: StepGenerate, WorkDir: t.TempDir(), ScratchDir: scratch,
		SessionID: "s-1", Prompt: "write", Timeout: 30 * time.Second,
	}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(scratch, "concepts/x.md")); err != nil {
		t.Errorf("expected file in the scratch dir: %v", err)
	}
}

func TestRunRejectsInvalidRequests(t *testing.T) {
	r := &ClaudeRunner{Binary: fakeClaude(t)}

	tests := []struct {
		name string
		req  Request
	}{
		{"no work dir", Request{Step: StepAnalyze, Prompt: "x"}},
		{"generate without scratch", Request{Step: StepGenerate, WorkDir: "/src", Prompt: "x"}},
		{"unknown step", Request{Step: "invent", WorkDir: "/src", Prompt: "x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), tt.req); err == nil {
				t.Error("Run accepted an invalid request")
			}
		})
	}
}

func TestParseResultTolerantOfLeadingNoise(t *testing.T) {
	// The CLI may emit warnings before the JSON object.
	raw := []byte("warning: something\n{\"subtype\":\"success\",\"is_error\":false,\"total_cost_usd\":0.5}")

	res, err := ParseResult(raw)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if !res.Succeeded() || res.TotalCostUSD != 0.5 {
		t.Errorf("parsed = %+v", res)
	}
}

func TestParseResultErrors(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json at all"} {
		if _, err := ParseResult([]byte(raw)); err == nil {
			t.Errorf("ParseResult(%q) succeeded", raw)
		}
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 10, CacheReadInputTokens: 5}
	if got := u.Total(); got != 165 {
		t.Errorf("Total() = %d, want 165", got)
	}
}

func TestResultDuration(t *testing.T) {
	r := Result{DurationMS: 1500}
	if got := r.Duration(); got != 1500*time.Millisecond {
		t.Errorf("Duration() = %v", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func flagVal(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
