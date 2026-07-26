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
			Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12},
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

	c = base()
	c.Agent.Runner = "carrier-pigeon"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "want api, cli, or fake") {
		t.Errorf("unknown-runner message must name all three kinds: %v", err)
	}
}

func TestValidateStorageCredentialPairing(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendS3, Bucket: "kiln"},
			Agent:    Agent{Binary: "claude", Timeout: time.Minute, MaxPagesPerRun: 12},
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
