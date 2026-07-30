package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The health command is what container healthchecks and Kubernetes probes run,
// so its exit behavior is the contract: zero only when the server says it is
// ready, non-zero for every other outcome including an unreachable server.
func TestHealthCommand(t *testing.T) {
	var probed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = r.URL.Path
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unready","reason":"database unreachable"}`))
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	run := func(args ...string) error {
		cmd := newHealthCmd()
		cmd.SetArgs(args)
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		return cmd.ExecuteContext(context.Background())
	}

	// Readiness is the default and reflects the database: an unready server
	// must fail the probe so it leaves the load balancer.
	err := run("--addr", srv.URL)
	if err == nil {
		t.Fatal("unready server passed the readiness probe")
	}
	if probed != "/readyz" {
		t.Errorf("probed %q, want /readyz by default", probed)
	}
	if !strings.Contains(err.Error(), "database unreachable") {
		t.Errorf("error %q does not carry the server's reason", err)
	}

	// Liveness answers even while the database is down -- that difference is
	// the whole reason an orchestrator does not restart a pod waiting on
	// Postgres.
	if err := run("--addr", srv.URL, "--live"); err != nil {
		t.Errorf("liveness probe failed against a live server: %v", err)
	}
	if probed != "/healthz" {
		t.Errorf("probed %q, want /healthz with --live", probed)
	}
}

func TestHealthCommandUnreachable(t *testing.T) {
	// A closed port stands in for a server that has not started: the probe
	// must fail rather than hang or report success.
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	cmd := newHealthCmd()
	cmd.SetArgs([]string{"--addr", addr, "--timeout", "2s"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("probe against a closed port succeeded")
	}
}
