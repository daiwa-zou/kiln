package config

import (
	"strings"
	"testing"
	"time"
)

// The exec mode decides whether kiln works behind a transaction-mode pooler at
// all, so a value pgx would accept but a pooler breaks is refused at startup
// rather than at the first query that lands on a different server connection.
func TestValidateQueryExecMode(t *testing.T) {
	base := func(mode string) *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2, QueryExecMode: mode},
			Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
			Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
		}
	}

	for _, mode := range []string{"", "cache_statement", "cache_describe", "exec"} {
		if err := base(mode).Validate(); err != nil {
			t.Errorf("query_exec_mode %q rejected: %v", mode, err)
		}
	}

	// describe_exec re-describes on every query and pgx documents it as
	// unsafe behind poolers; simple_protocol interpolates arguments client
	// side. Neither is a mode a pooled deployment should be able to pick.
	for _, mode := range []string{"describe_exec", "simple_protocol", "carrier-pigeon", "CACHE_DESCRIBE"} {
		err := base(mode).Validate()
		if err == nil {
			t.Errorf("query_exec_mode %q accepted", mode)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "query_exec_mode") || !strings.Contains(msg, "cache_describe") {
			t.Errorf("query_exec_mode %q: error %q should name the key and the allowed values", mode, msg)
		}
	}
}
