package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Validate checks the resolved configuration for combinations that would fail
// later at a less obvious point. It reports every problem at once, because
// fixing container configuration one error per restart is miserable.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.HTTPAddr) == "" {
		problems = append(problems, "http_addr is empty")
	}

	// A half-configured pair serves plain HTTP silently, which is the failure
	// nobody notices until a client refuses to connect.
	if (c.TLSCert == "") != (c.TLSKey == "") {
		problems = append(problems, "tls_cert and tls_key must be set together")
	}
	// public_url is what the MCP discovery documents advertise, and a client
	// that reads http:// there will refuse the connector. Behind a terminating
	// proxy this is the only place the real scheme is known.
	if u := strings.TrimSpace(c.PublicURL); u != "" && !strings.HasPrefix(u, "https://") &&
		!strings.HasPrefix(u, "http://localhost") && !strings.HasPrefix(u, "http://127.0.0.1") {
		problems = append(problems, fmt.Sprintf(
			"public_url must be https (or a loopback address for local use), got %q", u))
	}

	if c.Database.URL == "" {
		if c.Database.Host == "" {
			problems = append(problems, "database: neither url nor host is set")
		}
		if c.Database.Port <= 0 || c.Database.Port > 65535 {
			problems = append(problems, fmt.Sprintf("database: port %d out of range", c.Database.Port))
		}
		if c.Database.Name == "" {
			problems = append(problems, "database: name is empty")
		}
	}
	if c.Database.MaxConns < 1 {
		problems = append(problems, fmt.Sprintf("database: max_conns must be at least 1, got %d", c.Database.MaxConns))
	}
	if c.Database.MinConns < 0 {
		problems = append(problems, fmt.Sprintf("database: min_conns cannot be negative, got %d", c.Database.MinConns))
	}
	if c.Database.MinConns > c.Database.MaxConns {
		problems = append(problems, fmt.Sprintf(
			"database: min_conns (%d) exceeds max_conns (%d)", c.Database.MinConns, c.Database.MaxConns))
	}
	if m := c.Database.QueryExecMode; m != "" && !slices.Contains(ValidQueryExecModes, m) {
		problems = append(problems, fmt.Sprintf(
			"database: unknown query_exec_mode %q (want empty for the pgx default, or one of %s)",
			m, strings.Join(ValidQueryExecModes, ", ")))
	}

	switch c.Storage.Backend {
	case BackendS3:
		if c.Storage.Bucket == "" {
			problems = append(problems, "storage: bucket is required for the s3 backend")
		}
		// Credentials may legitimately be absent: minio-go falls through to the
		// IAM role / IRSA / instance-profile chain. Only a half-set pair is wrong.
		if (c.Storage.AccessKey == "") != (c.Storage.SecretKey == "") {
			problems = append(problems, "storage: access_key and secret_key must be set together, or both left unset to use the credential chain")
		}
	case BackendFS:
		if c.Storage.Path == "" {
			problems = append(problems, "storage: path is required for the fs backend")
		}
	default:
		problems = append(problems, fmt.Sprintf("storage: unknown backend %q (want s3 or fs)", c.Storage.Backend))
	}

	// Sanity checks for the runners kiln ships, and deliberately no rejection of
	// names it does not recognise. Whether a runner exists is a question only
	// the provider registry can answer, and internal/agent imports this package
	// so this one cannot ask it. An unknown name therefore fails when the runner
	// is built -- still at startup, with an error that lists the providers this
	// binary actually has, which is better than a list duplicated here and left
	// to drift.
	switch c.Agent.Runner {
	case RunnerAPI, "":
		// Empty means the API runner, matching what the factory does -- Load
		// always fills the default in, but a hand-built Config should not have
		// to restate it.
		//
		// An empty API key is also allowed: the SDK falls through to its own
		// credential resolution, so ambient configuration keeps working.
	case RunnerCLI:
		if c.Agent.Binary == "" {
			problems = append(problems, "agent: binary is required for the cli runner")
		}
	case RunnerFake:
		if c.Agent.FakeCostUSD < 0 {
			problems = append(problems, "agent: fake_cost_usd cannot be negative")
		}
	}

	switch c.Agent.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		problems = append(problems, fmt.Sprintf(
			"agent: unknown effort %q (want low, medium, high, xhigh, or max)", c.Agent.Effort))
	}
	if c.Agent.Timeout <= 0 {
		problems = append(problems, "agent: timeout must be positive")
	}
	if c.Agent.AnalyzeBudgetUSD < 0 || c.Agent.PageBudgetUSD < 0 || c.Agent.RunBudgetUSD < 0 {
		problems = append(problems, "agent: budgets cannot be negative")
	}
	// A per-call budget above the run ceiling is dead configuration, not a
	// tight one. The ledger reserves a call's budget before making the call,
	// so a reservation that cannot fit under the ceiling is refused every
	// time: the run fails on its first unit, forever, with an "exhausted"
	// error that points at the run budget rather than at the real culprit.
	// Cheaper to refuse the combination than to debug it at $0.01 a try.
	if c.Agent.RunBudgetUSD > 0 {
		for _, b := range []struct {
			key string
			usd float64
		}{
			{"analyze_budget_usd", c.Agent.AnalyzeBudgetUSD},
			{"page_budget_usd", c.Agent.PageBudgetUSD},
		} {
			if b.usd > c.Agent.RunBudgetUSD {
				problems = append(problems, fmt.Sprintf(
					"agent: %s ($%.2f) exceeds run_budget_usd ($%.2f), so no call could ever reserve its budget and every run would fail on its first unit",
					b.key, b.usd, c.Agent.RunBudgetUSD))
			}
		}
	}
	if c.Agent.UnitConcurrency < 1 {
		problems = append(problems, "agent: unit_concurrency must be at least 1")
	}
	if c.Agent.MaxPagesPerRun < 1 {
		problems = append(problems, "agent: max_pages_per_run must be at least 1")
	}

	if c.Worker.PollInterval < 0 {
		problems = append(problems, "worker: poll_interval cannot be negative")
	}
	if c.Worker.StaleAfter != 0 && c.Worker.StaleAfter <= c.Agent.Timeout {
		problems = append(problems, fmt.Sprintf(
			"worker: stale_after (%s) must exceed agent.timeout (%s), or an in-flight run would be requeued mid-build",
			c.Worker.StaleAfter, c.Agent.Timeout))
	}
	if c.Worker.HeartbeatInterval < 0 {
		problems = append(problems, "worker: heartbeat_interval cannot be negative")
	}
	// A heartbeat is what keeps a healthy long build from looking dead, so the
	// deadline has to survive a missed beat or two (a slow query, a brief
	// network partition) before another worker takes the run and builds it a
	// second time. Three intervals is two misses of slack.
	if c.Worker.StaleAfter != 0 && c.Worker.HeartbeatInterval > 0 &&
		c.Worker.StaleAfter < 3*c.Worker.HeartbeatInterval {
		problems = append(problems, fmt.Sprintf(
			"worker: stale_after (%s) must be at least three times heartbeat_interval (%s), or one missed heartbeat would let a second worker claim a live run",
			c.Worker.StaleAfter, c.Worker.HeartbeatInterval))
	}

	switch c.Auth.Mode {
	case AuthToken, AuthNone:
	case "":
		// A hand-built Config should not have to restate the default; Load
		// always fills it in.
	default:
		problems = append(problems, fmt.Sprintf("auth: unknown mode %q (want token or none)", c.Auth.Mode))
	}

	// "owner" is deliberately not accepted: a default that hands out
	// connector, credential and membership control to whoever is minted in
	// is never what a typo should produce. Matching is exact, so "Viewer"
	// fails here rather than silently granting the column default.
	switch c.Auth.DefaultRole {
	case "viewer", "member":
	case "":
		// As with mode: Load fills it in, and Mint treats empty as member.
	default:
		problems = append(problems, fmt.Sprintf("auth: unknown default_role %q (want viewer or member)", c.Auth.DefaultRole))
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("config: %w:\n  - %s",
		ErrInvalid, strings.Join(problems, "\n  - "))
}

// ErrInvalid wraps every validation failure so callers can distinguish bad
// configuration from an I/O problem while loading it.
var ErrInvalid = errors.New("invalid configuration")
