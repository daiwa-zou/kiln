package config

import "time"

// Pool sizes differ by role on purpose. The server issues many short queries;
// the worker holds a small number of long-lived transactions during import. A
// worker sized like a server would exhaust Postgres connections as replicas
// scale, for no benefit. The scheduler runs one duty query at a time per duty.
const (
	defaultServerMaxConns    int32 = 20
	defaultWorkerMaxConns    int32 = 6
	defaultSchedulerMaxConns int32 = 2
)

func applyDefaults(v setter, role Role) {
	v.SetDefault("http_addr", ":8080")
	// Loopback by default. The endpoint is unauthenticated and reports queue
	// depth and spend, so a bare ":9090" would hand that to every host that can
	// reach the machine. A deployment that needs Prometheus to scrape from
	// another host or pod opts in with metrics_addr = ":9090" (the Helm chart,
	// local-k8s manifests and docker-compose do).
	v.SetDefault("metrics_addr", "127.0.0.1:9090")
	v.SetDefault("log_level", "info")

	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.name", "kiln")
	v.SetDefault("database.user", "kiln")
	// require, not disable: a self-hosted deployment should have to opt out of
	// TLS deliberately rather than get plaintext by omission.
	v.SetDefault("database.sslmode", "require")
	v.SetDefault("database.min_conns", 2)
	v.SetDefault("database.max_conn_lifetime", time.Hour)
	v.SetDefault("database.max_conn_idle_time", 15*time.Minute)
	v.SetDefault("database.max_conns", defaultMaxConns(role))

	v.SetDefault("storage.backend", string(BackendS3))
	v.SetDefault("storage.region", "us-east-1")
	v.SetDefault("storage.bucket", "kiln")
	v.SetDefault("storage.use_ssl", true)
	v.SetDefault("storage.path_style", true)
	v.SetDefault("storage.path", "/var/lib/kiln/blobs")
	v.SetDefault("storage.run_artifact_retention", 30*24*time.Hour)
	v.SetDefault("storage.soft_delete_retention", 30*24*time.Hour)

	// The API runner is the default: no sandbox, no write guard, no path-escape
	// checks, and explicit prompt caching across units.
	v.SetDefault("agent.runner", string(RunnerAPI))
	v.SetDefault("agent.binary", "claude")
	v.SetDefault("agent.effort", "high")
	// Sonnet 5 by default; workspaces that warrant Opus 5 override it. Flipping
	// this default would silently multiply every workspace's bill.
	v.SetDefault("agent.model", "claude-sonnet-5")
	v.SetDefault("agent.analyze_model", "claude-sonnet-5")
	v.SetDefault("agent.fallback_model", "claude-sonnet-5")
	v.SetDefault("agent.timeout", 12*time.Minute)
	v.SetDefault("agent.analyze_budget_usd", 0.40)
	v.SetDefault("agent.page_budget_usd", 1.50)
	v.SetDefault("agent.run_budget_usd", 6.00)
	v.SetDefault("agent.max_pages_per_run", 12)
	v.SetDefault("agent.unit_concurrency", 1)
	v.SetDefault("agent.warn_turns", 40)
	// A month-shaped window: workspace budget_usd reads naturally as a
	// monthly cap. Enforced only for workspaces that set a budget.
	v.SetDefault("agent.budget_window", 30*24*time.Hour)
	// The fake runner charges a synthetic cent per call so cost plumbing
	// (ledger, trailing estimates, budget windows) is exercised in dev.
	v.SetDefault("agent.fake_cost_usd", 0.01)

	// Token auth by default: a shared deployment should have to opt out of
	// authentication deliberately rather than ship open by omission.
	v.SetDefault("auth.mode", string(AuthToken))
	// "member" is the role the column default has always granted, so an
	// upgrade changes nothing; a hosted deployment opts in to "viewer".
	v.SetDefault("auth.default_role", "member")

	// A month-long browser session; sessions are server-side rows, so a
	// stolen cookie is revocable regardless of this.
	v.SetDefault("github.session_ttl", 30*24*time.Hour)
	// One rebuild per minute per bench at most, however hard someone pushes.
	v.SetDefault("github.webhook_cooldown", time.Minute)

	v.SetDefault("worker.poll_interval", 5*time.Second)
	// Poll-triggered sources refresh every 10 minutes; the hash gate makes an
	// unchanged sweep free, so the cost of polling is one sync, not one build.
	v.SetDefault("worker.source_poll_interval", 10*time.Minute)
	// Above agent.timeout so a drain lets the current agent call finish.
	v.SetDefault("worker.drain_grace", 15*time.Minute)
	// Comfortably above agent.timeout, so only a genuinely dead worker's run is
	// requeued, never one still inside a slow model call.
	v.SetDefault("worker.stale_after", 45*time.Minute)
	// A heartbeat is one indexed single-row update. A minute is frequent enough
	// that stale_after can be tightened to minutes if an operator wants dead
	// workers recovered quickly, and rare enough to cost nothing.
	v.SetDefault("worker.heartbeat_interval", time.Minute)
	// Workers run the scheduler duties unless told a scheduler does, so
	// upgrading the image without the deployment never stops the sweeps.
	v.SetDefault("worker.scheduler_duties", true)
}

func defaultMaxConns(role Role) int32 {
	switch role {
	case RoleWorker:
		return defaultWorkerMaxConns
	case RoleScheduler:
		return defaultSchedulerMaxConns
	default:
		return defaultServerMaxConns
	}
}

// setter is the slice of viper this package depends on, kept narrow so defaults
// can be tested without constructing a full viper instance.
type setter interface {
	SetDefault(key string, value any)
}
