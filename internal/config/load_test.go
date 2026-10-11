package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateEnv points config discovery at an empty directory so a developer's
// real ~/.config/kiln/config.toml can never influence a test run.
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestLoadDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(Options{Role: RoleServer})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	// The metrics endpoint is unauthenticated, so it must not listen on every
	// interface unless a deployment says so.
	if cfg.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("MetricsAddr = %q, want 127.0.0.1:9090 (loopback only by default)", cfg.MetricsAddr)
	}
	if cfg.Database.SSLMode != "require" {
		t.Errorf("sslmode = %q, want require (TLS should be opt-out, not opt-in)", cfg.Database.SSLMode)
	}
	if cfg.Database.MaxConns != defaultServerMaxConns {
		t.Errorf("server max_conns = %d, want %d", cfg.Database.MaxConns, defaultServerMaxConns)
	}
	if cfg.Agent.Timeout != 12*time.Minute {
		t.Errorf("agent timeout = %v, want 12m", cfg.Agent.Timeout)
	}
}

// A deployment that scrapes from another pod or host must be able to opt back
// in to all interfaces.
func TestLoadMetricsAddrOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_METRICS_ADDR", ":9090")
	cfg, err := Load(Options{Role: RoleServer})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Errorf("MetricsAddr = %q, want :9090", cfg.MetricsAddr)
	}
}

func TestLoadWorkerRoleGetsSmallerPool(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(Options{Role: RoleWorker})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.MaxConns != defaultWorkerMaxConns {
		t.Errorf("worker max_conns = %d, want %d", cfg.Database.MaxConns, defaultWorkerMaxConns)
	}
}

func TestLoadEnvOverridesDefaults(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_HTTP_ADDR", ":9999")
	t.Setenv("KILN_DATABASE_MAX_CONNS", "42")
	t.Setenv("KILN_AGENT_MODEL", "opus")
	t.Setenv("KILN_AUTH_MODE", "none")

	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want :9999", cfg.HTTPAddr)
	}
	if cfg.Database.MaxConns != 42 {
		t.Errorf("max_conns = %d, want 42", cfg.Database.MaxConns)
	}
	if cfg.Agent.Model != "opus" {
		t.Errorf("agent model = %q, want opus", cfg.Agent.Model)
	}
	if cfg.Auth.Mode != AuthNone {
		t.Errorf("auth mode = %q, want none", cfg.Auth.Mode)
	}
}

// A pooled deployment sets the exec mode through the environment (compose and
// the Helm chart both do), so the key must bind without a config file.
func TestQueryExecModeEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_DATABASE_QUERY_EXEC_MODE", "cache_describe")

	cfg, err := Load(Options{Role: RoleWorker})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.QueryExecMode != "cache_describe" {
		t.Errorf("query_exec_mode = %q, want cache_describe from the environment", cfg.Database.QueryExecMode)
	}
}

// Adding pooler support must not move anyone who does not use a pooler: the
// exec mode stays pgx's own default and the pool sizes are what they were.
func TestPoolerDefaults(t *testing.T) {
	for _, tt := range []struct {
		role     Role
		maxConns int32
	}{
		{RoleServer, 20},
		{RoleWorker, 6},
	} {
		isolateEnv(t)
		cfg, err := Load(Options{Role: tt.role})
		if err != nil {
			t.Fatalf("Load(%s): %v", tt.role, err)
		}
		if cfg.Database.QueryExecMode != "" {
			t.Errorf("%s: query_exec_mode = %q, want empty (pgx default)", tt.role, cfg.Database.QueryExecMode)
		}
		if cfg.Database.MaxConns != tt.maxConns {
			t.Errorf("%s: max_conns = %d, want %d", tt.role, cfg.Database.MaxConns, tt.maxConns)
		}
		if cfg.Database.MinConns != 2 {
			t.Errorf("%s: min_conns = %d, want 2", tt.role, cfg.Database.MinConns)
		}
	}
}

// The default is the role the column default always granted, so an upgrade
// changes nothing; a deployment opts in to viewer through the environment.
func TestAuthDefaultRole(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.DefaultRole != "member" {
		t.Errorf("default_role = %q, want member", cfg.Auth.DefaultRole)
	}

	t.Setenv("KILN_AUTH_DEFAULT_ROLE", "viewer")
	cfg, err = Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.DefaultRole != "viewer" {
		t.Errorf("default_role = %q with KILN_AUTH_DEFAULT_ROLE=viewer, want viewer", cfg.Auth.DefaultRole)
	}

	// A typo stops startup rather than quietly minting members.
	t.Setenv("KILN_AUTH_DEFAULT_ROLE", "viewr")
	if _, err := Load(Options{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Load with default_role=viewr: err = %v, want ErrInvalid", err)
	}
}

func TestValidateDefaultRole(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
			Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
		}
	}

	for _, tc := range []struct {
		role string
		ok   bool
	}{
		{"", true}, // a hand-built Config; Load fills in member
		{"viewer", true},
		{"member", true},
		// owner would hand connector and membership control to anyone minted
		// in; matching is exact, so a capitalised or misspelt role is refused.
		{"owner", false},
		{"admin", false},
		{"Viewer", false},
		{"viewr", false},
	} {
		c := base()
		c.Auth.DefaultRole = tc.role
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("default_role %q rejected: %v", tc.role, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "default_role")) {
			t.Errorf("default_role %q: err = %v, want a default_role problem", tc.role, err)
		}
	}
}

func TestLoadFileOverriddenByEnv(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "http_addr = \":7000\"\n\n[agent]\nmodel = \"from-file\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Env must win over file.
	t.Setenv("KILN_AGENT_MODEL", "from-env")

	cfg, err := Load(Options{File: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTPAddr != ":7000" {
		t.Errorf("HTTPAddr = %q, want :7000 (from file)", cfg.HTTPAddr)
	}
	if cfg.Agent.Model != "from-env" {
		t.Errorf("agent model = %q, want from-env (env outranks file)", cfg.Agent.Model)
	}
}

// github.app_slug was removed because nothing read it. Configs written while
// it existed must keep loading, from the file and from the environment.
func TestLoadIgnoresRetiredAppSlug(t *testing.T) {
	isolateEnv(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[github]\napp_id = 42\napp_slug = \"my-app\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_GITHUB_APP_SLUG", "my-app")

	cfg, err := Load(Options{File: path})
	if err != nil {
		t.Fatalf("Load with retired app_slug: %v", err)
	}
	if cfg.GitHub.AppID != 42 {
		t.Errorf("AppID = %d, want 42", cfg.GitHub.AppID)
	}
}

func TestLoadExplicitMissingFileIsAnError(t *testing.T) {
	isolateEnv(t)

	_, err := Load(Options{File: filepath.Join(t.TempDir(), "absent.toml")})
	if err == nil {
		t.Fatal("Load succeeded with a missing explicit config file; want an error")
	}
}

func TestLoadMissingDiscoveredFileIsFine(t *testing.T) {
	isolateEnv(t)

	// The normal container case: no file anywhere, everything from env.
	if _, err := Load(Options{}); err != nil {
		t.Fatalf("Load with no config file: %v", err)
	}
}

func TestSecretFileSuffixWinsOverPlainEnv(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	// Trailing newline is near-universal in secret files and must be stripped.
	if err := os.WriteFile(path, []byte("from-file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KILN_MASTER_KEY", "from-plain-env")
	t.Setenv("KILN_MASTER_KEY_FILE", path)

	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Secrets.MasterKey != "from-file-secret" {
		t.Errorf("MasterKey = %q, want %q (the _FILE form must win and be trimmed)",
			cfg.Secrets.MasterKey, "from-file-secret")
	}
}

func TestSecretFilePlainEnvUsedWhenNoFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_ANTHROPIC_API_KEY", "sk-plain")

	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Secrets.AnthropicAPIKey != "sk-plain" {
		t.Errorf("AnthropicAPIKey = %q, want sk-plain", cfg.Secrets.AnthropicAPIKey)
	}
}

func TestSecretFileUnreadableIsAnError(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := Load(Options{})
	if err == nil {
		t.Fatal("Load succeeded with an unreadable _FILE secret; want an error")
	}
	if !strings.Contains(err.Error(), "MASTER_KEY_FILE") {
		t.Errorf("error should name the offending variable, got: %v", err)
	}
}

func TestDatabaseURLViaSecretFile(t *testing.T) {
	isolateEnv(t)

	path := filepath.Join(t.TempDir(), "dsn")
	dsn := "postgres://u:p@h:5432/d?sslmode=require"
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_DATABASE_URL_FILE", path)

	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Database.DSN(); got != dsn {
		t.Errorf("DSN() = %q, want %q", got, dsn)
	}
}

func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	cfg := &Config{
		Role:     RoleServer,
		HTTPAddr: "",
		Database: Database{Host: "", Port: 0, Name: "", MaxConns: 0, MinConns: -1},
		Storage:  Storage{Backend: "elsewhere"},
		Agent:    Agent{Binary: "", Timeout: 0, MaxPagesPerRun: 0},
		Auth:     Auth{Mode: "carrier-pigeon"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate passed on a thoroughly broken config")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error should wrap ErrInvalid, got %v", err)
	}

	// Fixing configuration one error per container restart is miserable, so
	// every problem must be reported in a single pass.
	for _, want := range []string{"http_addr", "database", "storage", "agent", "auth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q:\n%v", want, err)
		}
	}
}

func TestValidateFakeRunner(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
			Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
		}
	}

	if err := base().Validate(); err != nil {
		t.Errorf("fake runner rejected: %v", err)
	}

	c := base()
	c.Agent.FakeCostUSD = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "fake_cost_usd") {
		t.Errorf("negative fake cost accepted: %v", err)
	}

	// An unrecognised runner is deliberately NOT rejected here. Which runners
	// exist is a property of the provider registry in internal/agent, which
	// imports this package and so cannot be consulted from it; the name is
	// resolved when the runner is built, and the registry's error lists what
	// this binary actually has. Duplicating a list of kinds here is precisely
	// what would go stale the first time a provider is added.
	c = base()
	c.Agent.Runner = "carrier-pigeon"
	if err := c.Validate(); err != nil {
		t.Errorf("config rejected an unknown runner it cannot adjudicate: %v", err)
	}
}

// A per-call budget above the run ceiling can never be reserved, so every run
// dies on its first unit with an error naming the run budget rather than the
// per-call one that actually caused it. config.dev.toml shipped exactly this
// combination once ($1.50 page budget under a $1 run budget) and it made
// `make dev-build` impossible.
func TestValidateRejectsPerCallBudgetAboveRunBudget(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
			Agent: Agent{
				Runner: RunnerFake, Timeout: time.Minute,
				MaxPagesPerRun: 12, UnitConcurrency: 1,
				AnalyzeBudgetUSD: 0.40, PageBudgetUSD: 1.50, RunBudgetUSD: 6.00,
			},
		}
	}

	if err := base().Validate(); err != nil {
		t.Errorf("the shipped production budgets were rejected: %v", err)
	}

	c := base()
	c.Agent.RunBudgetUSD = 1.00
	err := c.Validate()
	if err == nil {
		t.Fatal("a page budget above the run budget was accepted")
	}
	// The message has to name the per-call key, since naming only the run
	// budget is what made this hard to diagnose in the first place.
	if !strings.Contains(err.Error(), "page_budget_usd") {
		t.Errorf("error does not name the offending key:\n%v", err)
	}

	c = base()
	c.Agent.RunBudgetUSD = 0.20
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "analyze_budget_usd") {
		t.Errorf("an analyze budget above the run budget was accepted: %v", err)
	}

	// Zero means unlimited, so nothing can exceed it.
	c = base()
	c.Agent.RunBudgetUSD = 0
	if err := c.Validate(); err != nil {
		t.Errorf("an unlimited run budget rejected per-call budgets: %v", err)
	}
}

func TestValidateStorageCredentialPairing(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendS3, Bucket: "kiln"},
			Agent:    Agent{Binary: "claude", Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
		}
	}

	t.Run("both unset uses the credential chain", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("both set is fine", func(t *testing.T) {
		c := base()
		c.Storage.AccessKey, c.Storage.SecretKey = "ak", "sk"
		if err := c.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("half set is rejected", func(t *testing.T) {
		c := base()
		c.Storage.AccessKey = "ak"
		if err := c.Validate(); err == nil {
			t.Error("Validate passed with an access key and no secret key")
		}
	})
}
