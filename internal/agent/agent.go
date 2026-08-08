// Package agent runs the sandboxed claude invocations that write page prose.
//
// Everything structural -- what to regenerate, the index, the log, validation --
// is decided deterministically elsewhere. The agent's only job is prose, and it
// is given the narrowest possible surface to do it: no Bash, no network tools,
// and write access to a scratch directory rather than the sources it reads.
package agent

import (
	"context"
	"time"
)

// Step distinguishes the calls the pipeline makes.
type Step string

const (
	// StepAnalyze investigates read-only and returns a structured plan. Its
	// output is validated before any money is spent on generation.
	StepAnalyze Step = "analyze"
	// StepGenerate writes pages, resuming the analyze session so the source
	// context stays prompt-cached between the two.
	StepGenerate Step = "generate"
	// StepResearch answers one review item against the whole corpus and
	// returns findings, writing nothing. It is not part of a generation pass:
	// a build's units each see one slice of the material, which is why they
	// raise questions they cannot settle, and this is the read that is not
	// bounded to a slice.
	StepResearch Step = "research"
)

// Request is one claude invocation.
type Request struct {
	Step Step
	// SessionID ties analyze and generate together on the CLI runner, which
	// resumes the session. The API runner is stateless and ignores it; the
	// pipeline compensates by passing the analysis plan explicitly in the
	// generate prompt.
	SessionID string
	// WorkDir is the process working directory: the materialized sources. The
	// installed CLI has no --cwd flag, so this is set on the command itself.
	WorkDir string
	// ScratchDir is the only writable location, granted via --add-dir.
	ScratchDir string

	Model         string
	FallbackModel string
	// BudgetUSD caps spend. The CLI enforces it itself via --max-budget-usd;
	// the API runner can only measure after the fact and sets
	// Result.OverBudget for the pipeline to surface.
	BudgetUSD float64
	// Timeout bounds wall-clock. The installed CLI has no --max-turns, so this
	// is the only hard stop on a runaway session.
	Timeout time.Duration

	SystemPrompt string
	Prompt       string
	// JSONSchema, when set, forces structured output. Used for analyze so the
	// plan needs no parsing heroics.
	JSONSchema string
	// CacheableContext is prompt content identical across every unit in a run --
	// the rendered map and the steering documents. APIRunner puts a cache
	// breakpoint after it, so all but the first unit reads it at a tenth of the
	// input rate. Must be byte-identical between units or nothing caches.
	CacheableContext string
	// MaxTokens caps the response. Zero uses the runner's default.
	MaxTokens int64
}

// Usage is the token accounting returned by the CLI.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Total returns all tokens billed, cached or not.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Result is the parsed --output-format json envelope.
type Result struct {
	IsError           bool               `json:"is_error"`
	Subtype           string             `json:"subtype"`
	StopReason        string             `json:"stop_reason"`
	TerminalReason    string             `json:"terminal_reason"`
	APIErrorStatus    *int               `json:"api_error_status"`
	SessionID         string             `json:"session_id"`
	NumTurns          int                `json:"num_turns"`
	TotalCostUSD      float64            `json:"total_cost_usd"`
	DurationMS        int64              `json:"duration_ms"`
	Usage             Usage              `json:"usage"`
	Result            string             `json:"result"`
	PermissionDenials []PermissionDenial `json:"permission_denials"`

	// Model names the model that actually served the request.
	Model string `json:"model,omitempty"`
	// OverBudget marks a call whose cost exceeded its per-call budget. The CLI
	// enforces its budget itself; the API runner can only measure after the
	// fact, so this is a flag for the pipeline to surface, not a hard stop.
	OverBudget bool `json:"-"`
	// Generation is set by runners that return page content as structured data
	// rather than writing files. ClaudeRunner leaves it nil and the pipeline
	// collects from the scratch directory instead; APIRunner populates it and
	// no filesystem is involved. This field is what lets both runners satisfy
	// one interface without the pipeline caring which is in use.
	Generation *GenerationResult `json:"-"`
	// Analysis is the structured plan from an analyze step, when the runner
	// returns one.
	Analysis *AnalysisResult `json:"-"`
	// Research is the findings from a research step, when the runner returns
	// one. Like Analysis, a runner that only returns envelope text leaves this
	// nil and the caller parses Result instead.
	Research *ResearchResult `json:"-"`
}

// PermissionDenial records a tool call the sandbox or hooks refused. The
// pipeline logs these per unit rather than swallowing them: a denial usually
// means the agent tried to do something the design forbids, which is worth
// seeing.
type PermissionDenial struct {
	ToolName string `json:"tool_name"`
	Reason   string `json:"reason,omitempty"`
}

// Succeeded reports whether the invocation completed cleanly. Every field is
// checked rather than just the exit code, because the CLI reports API-level
// failures inside a successfully-returned envelope.
func (r Result) Succeeded() bool {
	return !r.IsError && r.Subtype == "success" && r.APIErrorStatus == nil
}

// Runner executes a request. This is the seam that lets the whole pipeline be
// tested with a scripted fake binary, no network, and no API key.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}
