package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Tool sets. Bash is disallowed outright: Read/Grep/Glob/Write/Edit is enough
// to write wiki pages, and dropping Bash removes most of the attack surface in
// one move. Network tools are denied because research happens in Go, outside
// the sandbox, where it can be cached, audited, and cost-bounded.
var (
	analyzeTools  = []string{"Read", "Grep", "Glob"}
	generateTools = []string{"Read", "Grep", "Glob", "Write", "Edit"}
	deniedTools   = []string{"Bash", "WebSearch", "WebFetch", "Task", "NotebookEdit", "TodoWrite"}
)

// ClaudeRunner invokes the claude CLI.
type ClaudeRunner struct {
	// Binary is the executable name or path. Resolved via PATH when bare.
	Binary string
	// Env, when non-nil, replaces the child environment entirely. Production
	// construction (agent.New) always sets it to MinimalChildEnv, so KILN_*
	// secrets and connector credentials never reach a process that reads
	// untrusted source content. A nil Env inherits the parent environment;
	// only tests, which drive fakeclaude through KILN_FAKE_SCRIPT, rely on that.
	Env []string
}

// Capabilities: pages arrive in the scratch directory rather than as data, and
// the CLI reports its own spend, so nothing here is estimated.
func (r *ClaudeRunner) Capabilities() Capabilities {
	return Capabilities{WritesFiles: true, EstimatesCost: false}
}

// NewClaudeRunner constructs a runner, defaulting the binary name.
func NewClaudeRunner(binary string) *ClaudeRunner {
	if strings.TrimSpace(binary) == "" {
		binary = "claude"
	}
	return &ClaudeRunner{Binary: binary}
}

// MinimalChildEnv builds the environment for the claude subprocess: enough for
// the binary to run and authenticate, and nothing else. The child processes
// untrusted source content, so the parent's KILN_* secrets (master key, session
// secret, database password) must never be inherited by it.
//
// HOME is passed so a logged-in CLI session's credentials still resolve when no
// API key is configured.
func MinimalChildEnv(apiKey string) []string {
	env := make([]string, 0, 6)
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "TERM", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if apiKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	}
	return env
}

// Run executes one request and parses the result envelope.
func (r *ClaudeRunner) Run(ctx context.Context, req Request) (*Result, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := BuildArgs(req)

	cmd := exec.CommandContext(ctx, r.Binary, args...)
	// There is no --cwd flag on the installed CLI; the working directory is
	// how the agent is pointed at the materialized sources.
	cmd.Dir = req.WorkDir
	if r.Env != nil {
		cmd.Env = r.Env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return nil, fmt.Errorf("agent: %s step exceeded its %s timeout", req.Step, req.Timeout)
	}

	// A non-zero exit still often carries a parseable envelope describing why,
	// which is more useful than the exit code alone.
	res, parseErr := ParseResult(stdout.Bytes())
	if parseErr != nil {
		if err != nil {
			return nil, fmt.Errorf("agent: %s step failed: %w (stderr: %s)",
				req.Step, err, truncate(stderr.String(), 500))
		}
		return nil, fmt.Errorf("agent: %s step returned unparseable output: %w", req.Step, parseErr)
	}
	if err != nil && res.Succeeded() {
		// Exit code and envelope disagree; trust the exit code.
		return res, fmt.Errorf("agent: %s step exited non-zero despite a success envelope: %w", req.Step, err)
	}
	return res, nil
}

func (req Request) validate() error {
	switch {
	case req.WorkDir == "":
		return errors.New("agent: WorkDir is required")
	case strings.TrimSpace(req.Prompt) == "":
		return errors.New("agent: Prompt is required")
	case req.Step == StepGenerate && req.ScratchDir == "":
		return errors.New("agent: ScratchDir is required for the generate step")
	case req.Step != StepAnalyze && req.Step != StepGenerate:
		return fmt.Errorf("agent: unknown step %q", req.Step)
	}
	return nil
}

// BuildArgs assembles the CLI arguments. Exported so tests can assert on the
// exact invocation without executing anything.
//
// Flags here were verified against the installed CLI. Two absences shape the
// design: there is no --max-turns (so Timeout is the only hard stop) and no
// --cwd (so the working directory is set on the command).
func BuildArgs(req Request) []string {
	args := []string{
		"-p",
		"--output-format", "json",
		// Isolate from source-supplied configuration. Ingested repos carry
		// .claude directories and CLAUDE.md files, and on a multi-user platform
		// those are untrusted input from another user -- a live prompt-injection
		// vector. These two flags stop the CLI loading any of it.
		"--setting-sources", "",
		"--strict-mcp-config",
		// Daemon runs should not pollute the operator's resume history.
		"--no-session-persistence",
	}

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.BudgetUSD, 'f', -1, 64))
	}
	// CacheableContext rides the system prompt: the CLI manages its own prompt
	// caching, and dropping the rendered map here would mean the agent never
	// sees the workspace overview at all.
	system := req.SystemPrompt
	if req.CacheableContext != "" {
		if system != "" {
			system += "\n\n"
		}
		system += req.CacheableContext
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}

	switch req.Step {
	case StepAnalyze:
		args = append(args,
			"--tools", strings.Join(analyzeTools, ","),
			"--permission-mode", "dontAsk",
		)
		if req.JSONSchema != "" {
			args = append(args, "--json-schema", req.JSONSchema)
		}
		if req.SessionID != "" {
			args = append(args, "--session-id", req.SessionID)
		}

	case StepGenerate:
		args = append(args,
			"--tools", strings.Join(generateTools, ","),
			"--disallowedTools", strings.Join(deniedTools, ","),
			// Grants the scratch directory while cwd stays the sources, so the
			// agent cannot write into the material it is documenting.
			"--add-dir", req.ScratchDir,
			"--permission-mode", "acceptEdits",
		)
		if req.FallbackModel != "" {
			args = append(args, "--fallback-model", req.FallbackModel)
		}
		if req.SessionID != "" {
			// Resume the analyze session so source context stays prompt-cached.
			args = append(args, "--resume", req.SessionID)
		}
	}

	return append(args, req.Prompt)
}

// ParseResult decodes the --output-format json envelope. The CLI may emit
// warnings before the JSON, so the object is located rather than assumed to
// start at byte zero.
func ParseResult(out []byte) (*Result, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("empty output")
	}

	if start := bytes.IndexByte(trimmed, '{'); start > 0 {
		trimmed = trimmed[start:]
	}

	var res Result
	if err := json.Unmarshal(trimmed, &res); err != nil {
		return nil, fmt.Errorf("decode envelope: %w (output: %s)", err, truncate(string(trimmed), 300))
	}
	return &res, nil
}

// Err returns a descriptive error when the result represents a failure, and nil
// otherwise, so callers can branch on one value.
func (r Result) Err() error {
	if r.Succeeded() {
		return nil
	}
	var parts []string
	if r.Subtype != "" && r.Subtype != "success" {
		parts = append(parts, "subtype="+r.Subtype)
	}
	if r.TerminalReason != "" && r.TerminalReason != "completed" {
		parts = append(parts, "terminal_reason="+r.TerminalReason)
	}
	if r.APIErrorStatus != nil {
		parts = append(parts, "api_error_status="+strconv.Itoa(*r.APIErrorStatus))
	}
	if r.Result != "" {
		parts = append(parts, truncate(r.Result, 200))
	}
	if len(parts) == 0 {
		parts = append(parts, "unspecified failure")
	}
	return fmt.Errorf("agent: %s", strings.Join(parts, "; "))
}

// Duration returns the wall-clock time the invocation took.
func (r Result) Duration() time.Duration {
	return time.Duration(r.DurationMS) * time.Millisecond
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
