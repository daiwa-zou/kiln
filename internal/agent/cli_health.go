package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLIHealth is what can be learned about the claude CLI without spending
// anything: that it is installed, what version it is, and whether it can
// authenticate.
type CLIHealth struct {
	// Path is the resolved executable, so an operator can see which of several
	// installs is actually being used.
	Path string
	// Version is what `claude --version` reported.
	Version string
	// LoggedIn is the CLI's own answer, from `claude auth status`. False with
	// no AuthErr means the CLI ran and said it cannot authenticate.
	LoggedIn bool
	// AuthMethod is how it authenticates when it can -- an API key, a
	// subscription session, or a cloud provider's credentials.
	AuthMethod string
	// AuthErr is set when the authentication state could not be determined at
	// all, which is different from determining that it is unauthenticated. An
	// older CLI without `auth status` lands here, and that is not a failure:
	// the runner may still work.
	AuthErr error
}

// Usable reports whether a build is likely to get past authentication.
//
// Likely, not certainly, and the gap is worth stating. `auth status` answers
// from the stored session without contacting anyone, so a session whose access
// token has not yet expired reports as logged in even when the refresh token
// behind it is dead -- which is what happens when the same credential is used
// from two places, since refresh tokens rotate on use. The first real call then
// fails, and the CLI marks the session logged out only at that point.
//
// This was observed rather than reasoned about: a worker reported
// subscriptionType "pro" and loggedIn true, failed its next build with "OAuth
// session expired and could not be refreshed", and reported logged out
// thereafter.
//
// So a false here is conclusive and a true is not. ProbeCLI is the definitive
// answer, at the cost of one small call.
func (h CLIHealth) Usable() bool { return h.LoggedIn }

// Summary is a one-line description for an operator.
func (h CLIHealth) Summary() string {
	switch {
	case h.AuthErr != nil:
		return fmt.Sprintf("%s (auth state unknown: %v)", h.Version, h.AuthErr)
	case h.LoggedIn:
		return fmt.Sprintf("%s, authenticated via %s", h.Version, h.AuthMethod)
	default:
		return h.Version + ", NOT authenticated -- run `claude auth login`"
	}
}

// checkTimeout bounds both probes. Neither makes a model call, so anything
// slower than this means the binary is wedged rather than working.
const checkTimeout = 20 * time.Second

// CheckCLI inspects the claude CLI without making a model call.
//
// This exists because the failure it catches is expensive and late. A worker
// configured for the CLI runner with no logged-in session accepts a run, plans
// it, and then fails every unit on authentication -- after the queue has
// claimed it and a human has watched it start. The two questions that predict
// that ("is the binary there" and "can it authenticate") are both answerable
// for free, in under a second, before anything is enqueued.
//
// The child environment is MinimalChildEnv, the same one a real invocation
// gets. That is load-bearing rather than tidy: the runner deliberately hands
// the subprocess a narrow environment so it cannot inherit KILN_* secrets, and
// a check run under the parent's full environment could pass on a credential
// the actual build would never see.
func CheckCLI(ctx context.Context, binary, apiKey, baseURL string) (CLIHealth, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "claude"
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return CLIHealth{}, fmt.Errorf("agent: %q is not on PATH: %w", binary, err)
	}
	h := CLIHealth{Path: path}

	env := MinimalChildEnv(apiKey, baseURL)

	version, err := runCLI(ctx, path, env, "--version")
	if err != nil {
		return h, fmt.Errorf("agent: %s could not be executed: %w", path, err)
	}
	h.Version = strings.TrimSpace(version)

	// `auth status` reports the CLI's own view, which is the only authority
	// worth asking: kiln cannot tell a valid session from an expired one by
	// looking at a credentials file, and on macOS there is no file to look at.
	//
	// Its exit code is deliberately ignored in favour of its output. Logged out
	// is reported as exit 1 *with* a well-formed answer on stdout, so treating
	// a non-zero exit as failure turns the one state this check exists to catch
	// into "state unknown" -- which is precisely the reading that would let a
	// worker start anyway. The CLI runner already encodes the same lesson about
	// the result envelope; this is that rule applied to the same tool's other
	// subcommand.
	raw, runErr := runCLI(ctx, path, env, "auth", "status", "--json")

	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &status); err != nil {
		// Only now does the exit status matter: no parseable answer and a
		// failed run means the subcommand is missing or broken, which is
		// genuinely unknown rather than unauthenticated.
		if runErr != nil {
			h.AuthErr = runErr
		} else {
			h.AuthErr = fmt.Errorf("could not read `auth status` output: %w", err)
		}
		return h, nil
	}
	h.LoggedIn = status.LoggedIn
	h.AuthMethod = status.AuthMethod
	return h, nil
}

// runCLI returns stdout whether or not the command succeeded. Callers decide
// what a non-zero exit means, because for `auth status` it carries an answer
// rather than the absence of one.
// ProbeCLI makes one real, minimal model call and reports whether it worked.
//
// This is the only check that actually answers the question. CheckCLI reads the
// CLI's stored opinion of itself, which can be stale in the one direction that
// matters; a call is what discovers that the session behind it is dead. It is
// separate, and opt-in at the callers, because it spends money -- a health
// check that quietly bills is a bad health check.
//
// Deliberately the cheapest possible request: no tools, no system prompt, a
// four-token answer, and a budget that stops it going anywhere.
func ProbeCLI(ctx context.Context, binary, apiKey, baseURL string) error {
	h, err := CheckCLI(ctx, binary, apiKey, baseURL)
	if err != nil {
		return err
	}

	r := &ClaudeRunner{Binary: h.Path, Env: MinimalChildEnv(apiKey, baseURL)}
	res, err := r.Run(ctx, Request{
		Step:      StepAnalyze,
		WorkDir:   os.TempDir(),
		Prompt:    "Reply with the single word OK.",
		BudgetUSD: 0.05,
		Timeout:   probeTimeout,
	})
	if err != nil {
		return fmt.Errorf("agent: the claude CLI could not complete a request: %w", err)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("agent: the claude CLI could not complete a request: %w", err)
	}
	return nil
}

// probeTimeout is generous next to checkTimeout: this one really does wait on a
// model.
const probeTimeout = 90 * time.Second

func runCLI(ctx context.Context, path string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w (%s)", err, truncate(strings.TrimSpace(string(ee.Stderr)), 200))
		}
		return string(out), err
	}
	return string(out), nil
}
