package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubCLI writes an executable standing in for the claude binary. A script
// rather than a mock because CheckCLI's whole job is to run a real process and
// interpret what comes back -- exit code included.
func stubCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckCLIReportsAnAuthenticatedInstall(t *testing.T) {
	cli := stubCLI(t, `
case "$1" in
  --version) echo "2.1.224 (Claude Code)" ;;
  auth) echo '{"loggedIn":true,"authMethod":"subscription"}' ;;
esac
`)

	h, err := CheckCLI(context.Background(), cli, "", "")
	if err != nil {
		t.Fatalf("CheckCLI: %v", err)
	}
	if !h.Usable() {
		t.Errorf("a logged-in CLI was reported unusable: %+v", h)
	}
	if h.Version != "2.1.224 (Claude Code)" {
		t.Errorf("Version = %q", h.Version)
	}
	if h.AuthMethod != "subscription" {
		t.Errorf("AuthMethod = %q, want subscription", h.AuthMethod)
	}
}

// The case this check exists for, and the one that is easy to get wrong:
// `claude auth status` exits 1 when logged out but still prints a well-formed
// answer on stdout. Reading the exit code instead of the output turns "not
// authenticated" -- the single state worth catching -- into "state unknown",
// which is exactly the reading that lets a worker start and fail every unit.
func TestCheckCLIReadsLoggedOutFromOutputNotExitCode(t *testing.T) {
	cli := stubCLI(t, `
case "$1" in
  --version) echo "2.1.224 (Claude Code)" ;;
  auth) echo '{"loggedIn":false,"authMethod":"none"}'; exit 1 ;;
esac
`)

	h, err := CheckCLI(context.Background(), cli, "", "")
	if err != nil {
		t.Fatalf("CheckCLI: %v", err)
	}
	if h.AuthErr != nil {
		t.Errorf("a definite 'not logged in' was recorded as unknown: %v", h.AuthErr)
	}
	if h.Usable() {
		t.Error("a logged-out CLI was reported usable")
	}
	if !strings.Contains(h.Summary(), "claude auth login") {
		t.Errorf("summary %q does not say how to fix it", h.Summary())
	}
}

// An older CLI has no `auth status`. That is genuinely unknown rather than
// unauthenticated, and must not be reported as a failure: the runner may work
// perfectly well.
func TestCheckCLIDistinguishesUnknownFromUnauthenticated(t *testing.T) {
	cli := stubCLI(t, `
case "$1" in
  --version) echo "1.0.0 (Claude Code)" ;;
  auth) echo "unknown command: auth" >&2; exit 127 ;;
esac
`)

	h, err := CheckCLI(context.Background(), cli, "", "")
	if err != nil {
		t.Fatalf("CheckCLI: %v", err)
	}
	if h.AuthErr == nil {
		t.Error("a CLI with no auth subcommand was reported as a definite answer")
	}
	if !strings.Contains(h.Summary(), "unknown") {
		t.Errorf("summary %q should say the auth state could not be determined", h.Summary())
	}
}

// The probe is the only conclusive answer, because `auth status` can report a
// session as live when the refresh token behind it is dead. Here the stored
// status says logged in and the call fails anyway -- the exact shape observed
// in a cluster whose credential had been used from a second place.
func TestProbeCLICatchesWhatAuthStatusMisses(t *testing.T) {
	cli := stubCLI(t, `
case "$1" in
  --version) echo "2.1.224 (Claude Code)" ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}' ;;
  *) echo '{"is_error":true,"subtype":"error_during_execution","terminal_reason":"api_error","result":"Failed to authenticate: OAuth session expired and could not be refreshed"}' ;;
esac
`)

	// The cheap check is satisfied...
	h, err := CheckCLI(context.Background(), cli, "", "")
	if err != nil {
		t.Fatalf("CheckCLI: %v", err)
	}
	if !h.Usable() {
		t.Fatal("precondition: the stored status should look healthy")
	}

	// ...and the call is not.
	err = ProbeCLI(context.Background(), cli, "", "")
	if err == nil {
		t.Fatal("a CLI that cannot complete a request was reported healthy")
	}
	if !strings.Contains(err.Error(), "could not complete a request") {
		t.Errorf("error %q should say the request failed", err)
	}
}

func TestProbeCLISucceedsAgainstAWorkingCLI(t *testing.T) {
	cli := stubCLI(t, `
case "$1" in
  --version) echo "2.1.224 (Claude Code)" ;;
  auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}' ;;
  *) echo '{"subtype":"success","terminal_reason":"completed","total_cost_usd":0.01,"num_turns":1,"result":"OK"}' ;;
esac
`)

	if err := ProbeCLI(context.Background(), cli, "", ""); err != nil {
		t.Errorf("a working CLI was reported unhealthy: %v", err)
	}
}

func TestCheckCLIFailsWhenTheBinaryIsMissing(t *testing.T) {
	_, err := CheckCLI(context.Background(), "definitely-not-installed-claude", "", "")
	if err == nil {
		t.Fatal("a missing binary was accepted")
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("error %q should say the binary was not found on PATH", err)
	}
}

func TestCheckCLIFailsWhenTheBinaryCannotRun(t *testing.T) {
	cli := stubCLI(t, `exit 3`)
	if _, err := CheckCLI(context.Background(), cli, "", ""); err == nil {
		t.Fatal("a binary that cannot report its version was accepted")
	}
}

// The check must run under the same narrow environment a real invocation gets.
// Under the parent's full environment it could pass on a credential the build
// would never see, which is a worse failure than not checking at all.
func TestCheckCLIRunsUnderTheMinimalChildEnvironment(t *testing.T) {
	t.Setenv("KILN_MASTER_KEY", "must-not-leak")

	envLog := filepath.Join(t.TempDir(), "env.txt")
	cli := stubCLI(t, `
case "$1" in
  --version) echo "2.1.224 (Claude Code)" ;;
  auth) env > `+envLog+`; echo '{"loggedIn":true,"authMethod":"apiKey"}' ;;
esac
`)

	if _, err := CheckCLI(context.Background(), cli, "sk-test", "https://gateway.example.com"); err != nil {
		t.Fatalf("CheckCLI: %v", err)
	}

	raw, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, "KILN_MASTER_KEY") {
		t.Error("a KILN_* secret reached the probe's environment")
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-test") {
		t.Error("the configured API key did not reach the probe, so it checked the wrong credential")
	}
	if !strings.Contains(got, "ANTHROPIC_BASE_URL=https://gateway.example.com") {
		t.Error("the configured base URL did not reach the probe, so it checked the wrong endpoint")
	}
}
