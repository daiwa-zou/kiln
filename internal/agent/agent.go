// Package agent runs the sandboxed claude invocations that write page prose.
//
// Everything structural -- what to regenerate, the index, the log, validation --
// is decided deterministically elsewhere. The agent's only job is prose, and it
// is given the narrowest possible surface to do it: no Bash, no network tools,
// and write access to a scratch directory rather than the sources it reads.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Step distinguishes the calls the pipeline makes.
type Step string

const (
	// StepAnalyze investigates read-only and returns a structured plan. Its
	// output is validated before any money is spent on generation.
	StepAnalyze Step = "analyze"
	// StepGenerate writes pages. On the CLI runner it resumes the analyze
	// session -- forked, so every attempt starts from the analysis alone -- and
	// the source material is therefore not repeated in its prompt.
	StepGenerate Step = "generate"
	// StepResearch answers one review item against the whole corpus and
	// returns findings, writing nothing. It is not part of a generation pass:
	// a build's units each see one slice of the material, which is why they
	// raise questions they cannot settle, and this is the read that is not
	// bounded to a slice.
	StepResearch Step = "research"
)

// Valid reports whether s is a step the pipeline can make. This is the only
// list of steps: every runner validates through Request.Validate, which asks
// here, so a step added above is accepted by all of them or by none. The
// switches in BuildArgs and schemaFor decide what each step *does*, but they
// can assume a valid step precisely because nothing reaches them without
// passing this first.
func (s Step) Valid() bool {
	switch s {
	case StepAnalyze, StepGenerate, StepResearch:
		return true
	}
	return false
}

// Request is one claude invocation.
type Request struct {
	Step Step
	// SessionID ties analyze and generate together on the CLI runner, which
	// resumes the session (see BuildArgs for why every generate attempt forks
	// it). The API runner is stateless and ignores it; the pipeline compensates
	// by passing the analysis plan explicitly in the generate prompt.
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
	// CacheableContext is prompt content identical across every unit in a run --
	// the rendered map and the steering documents. APIRunner puts a cache
	// breakpoint after it, so all but the first unit reads it at a tenth of the
	// input rate. Must be byte-identical between units or nothing caches.
	CacheableContext string
	// CacheableUnit is the unit's source material, for runners that report
	// CachesUnit. APIRunner sends it as the first content block of the user
	// message and puts a cache breakpoint after it, ahead of the step
	// instructions in Prompt, so analyze, generate and every generate retry of
	// one unit share a cached prefix. It must be byte-identical across those
	// calls or nothing caches.
	//
	// Runners that do not report CachesUnit never see a populated value: the
	// pipeline puts the material in Prompt for them instead, because a runner
	// that ignored this field would be sent no source at all. ClaudeRunner is
	// one of those -- it has its own session to resume, and the material rides
	// the analyze prompt only.
	CacheableUnit string
	// MaxTokens caps the response. Zero uses the runner's default.
	MaxTokens int64
}

// Validate checks the request against the contract every runner shares, as
// adjusted for what this kind of runner needs, which it states through c.
//
// Three runners used to carry their own copy of this and the copies drifted: the
// CLI runner rejected an empty WorkDir the others accepted, and rejected the
// research step the API runner accepted -- each only discovered in production,
// because the test doubles in front of them validated nothing. One function,
// called by every runner and every double, is what stops that recurring.
//
// What a request must carry depends on whether the runner works on files:
//
//   - ReadsSources: the agent explores the material itself, from WorkDir, so a
//     missing WorkDir is an error rather than a request that quietly runs in the
//     wrong directory.
//   - WritesFiles: the generate step writes its pages into ScratchDir.
//
// A runner that takes everything in the prompt and returns data (the API runner)
// needs neither, and an empty WorkDir or ScratchDir is simply ignored.
//
// Checks run in a fixed order so the same bad request always reports the same
// error, and the messages are the ones the CLI runner has always produced.
func (req Request) Validate(c Capabilities) error {
	switch {
	case c.ReadsSources && req.WorkDir == "":
		return errors.New("agent: WorkDir is required")
	case strings.TrimSpace(req.Prompt) == "":
		return errors.New("agent: Prompt is required")
	case c.WritesFiles && req.Step == StepGenerate && req.ScratchDir == "":
		return errors.New("agent: ScratchDir is required for the generate step")
	case !req.Step.Valid():
		return fmt.Errorf("agent: unknown step %q", req.Step)
	}
	return nil
}

// CachesUnit reports whether a runner takes the unit's source material in
// Request.CacheableUnit, and caches it, rather than reading it from Prompt.
//
// It is an opt-in separate from Capabilities because the default has to be the
// safe one: a runner that has never heard of CacheableUnit and is sent a request
// with the source moved there would write pages from a prompt that names the
// unit and contains none of it. The in-tree API-shaped runners (APIRunner and
// FakeRunner) say yes; everything else is handed the material in Prompt as it
// always was.
func CachesUnit(r Runner) bool {
	c, ok := r.(interface{ CachesUnit() bool })
	return ok && c.CachesUnit()
}

// StandInCapabilities is what a runner that merely stands in for the real ones
// -- FakeRunner, and the scripted doubles in tests -- validates against.
//
// It requires a WorkDir, which is what the CLI runner requires and the most
// common way a request has been wrong, but not a ScratchDir: the pipeline hands
// one only to runners that declare WritesFiles, and a stand-in is used on both
// sides of that line. Holding it to the strictest rule on every other axis means
// a pipeline change that would be rejected by the CLI runner fails a test here
// instead of in production.
var StandInCapabilities = Capabilities{ReadsSources: true}

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
