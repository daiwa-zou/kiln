package config

import (
	"errors"
	"fmt"
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

	if c.Agent.Binary == "" {
		problems = append(problems, "agent: binary is empty")
	}
	if c.Agent.Timeout <= 0 {
		problems = append(problems, "agent: timeout must be positive")
	}
	if c.Agent.AnalyzeBudgetUSD < 0 || c.Agent.PageBudgetUSD < 0 || c.Agent.RunBudgetUSD < 0 {
		problems = append(problems, "agent: budgets cannot be negative")
	}
	if c.Agent.MaxPagesPerRun < 1 {
		problems = append(problems, "agent: max_pages_per_run must be at least 1")
	}

	if c.Worker.Concurrency < 1 {
		problems = append(problems, "worker: concurrency must be at least 1")
	}
	if c.Worker.SweepJitter < 0 {
		problems = append(problems, "worker: sweep_jitter cannot be negative")
	}
	if c.Worker.SweepJitter >= c.Worker.SweepInterval && c.Worker.SweepInterval > 0 {
		problems = append(problems, "worker: sweep_jitter must be smaller than sweep_interval")
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
