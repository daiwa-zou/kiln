package agent

import (
	"fmt"

	"github.com/daiwa-zou/kiln/internal/config"
)

// New builds the runner selected by configuration.
//
// Both runners satisfy the same interface, so nothing downstream branches on
// which is in use. They differ in how page content comes back: the API runner
// returns it as structured data on Result.Generation, while the CLI runner
// writes files into the scratch directory for the caller to collect.
func New(cfg *config.Config) (Runner, error) {
	switch cfg.Agent.Runner {
	case config.RunnerAPI, "":
		return NewAPIRunner(APIOptions{
			APIKey:  cfg.Secrets.AnthropicAPIKey,
			BaseURL: cfg.Agent.BaseURL,
			Model:   cfg.Agent.Model,
			Effort:  cfg.Agent.Effort,
		}), nil

	case config.RunnerCLI:
		r := NewClaudeRunner(cfg.Agent.Binary)
		// The subprocess reads untrusted source content; give it an explicit
		// minimal environment so it can never inherit KILN_* secrets.
		r.Env = MinimalChildEnv(cfg.Secrets.AnthropicAPIKey)
		return r, nil

	default:
		return nil, fmt.Errorf("agent: unknown runner %q", cfg.Agent.Runner)
	}
}

// WritesFiles reports whether a runner produces pages on disk rather than as
// structured data. The pipeline uses this to decide whether it needs a scratch
// directory at all -- the API runner needs none, which is most of why it exists.
func WritesFiles(r Runner) bool {
	_, isCLI := r.(*ClaudeRunner)
	return isCLI
}

// EstimatesCost reports whether a runner's TotalCostUSD comes from the local
// pricing table rather than being reported authoritatively by the CLI. Budget
// enforcement needs to know: an estimated cost of zero means "unknown model",
// not "free".
func EstimatesCost(r Runner) bool {
	_, isAPI := r.(*APIRunner)
	return isAPI
}
