package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// EnvPrefix namespaces every environment variable kiln reads.
const EnvPrefix = "KILN"

// Options controls loading. Zero value is valid: server role, config file
// discovered from the standard locations.
type Options struct {
	Role Role
	// File, when set, is used instead of searching the default locations. A
	// missing file at an explicit path is an error; a missing discovered file
	// is not.
	File string
}

// Load resolves configuration from defaults, an optional file, and the
// environment, then reads any _FILE-indirected secrets from disk.
func Load(opts Options) (*Config, error) {
	role := opts.Role
	if role == "" {
		role = RoleServer
	}

	v := viper.New()
	applyDefaults(v, role)

	v.SetEnvPrefix(EnvPrefix)
	// KILN_DATABASE_MAX_CONNS -> database.max_conns
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := bindEnv(v); err != nil {
		return nil, err
	}

	if err := readConfigFile(v, opts.File); err != nil {
		return nil, err
	}

	// Secrets are resolved before unmarshalling: some of them (the database URL
	// and password) feed viper keys, so setting them afterwards would silently
	// have no effect on the decoded struct.
	secrets, err := loadSecrets(v)
	if err != nil {
		return nil, err
	}

	cfg := &Config{Role: role}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	cfg.Role = role
	cfg.Secrets = secrets
	// The storage secret key is both a normal setting and a secret; the _FILE
	// form takes precedence when present.
	if secrets.StorageSecretKey != "" {
		cfg.Storage.SecretKey = secrets.StorageSecretKey
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readConfigFile loads an explicit path, or searches the standard locations.
func readConfigFile(v *viper.Viper, explicit string) error {
	if explicit != "" {
		v.SetConfigFile(explicit)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("config: read %s: %w", explicit, err)
		}
		return nil
	}

	v.SetConfigName("config")
	v.SetConfigType("toml")
	for _, dir := range searchPaths() {
		v.AddConfigPath(dir)
	}

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if ok := asConfigFileNotFound(err, &notFound); ok {
			// Running purely from environment is the normal container case.
			return nil
		}
		return fmt.Errorf("config: read: %w", err)
	}
	return nil
}

func asConfigFileNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if e, ok := err.(viper.ConfigFileNotFoundError); ok {
		*target = e
		return true
	}
	return false
}

func searchPaths() []string {
	var paths []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "kiln"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "kiln"))
	}
	return append(paths, "/etc/kiln", ".")
}

// bindEnv registers every key explicitly. AutomaticEnv alone does not populate
// keys that are absent from a config file, which is exactly the container case.
func bindEnv(v *viper.Viper) error {
	keys := []string{
		"http_addr", "public_url", "log_level",
		"database.url", "database.host", "database.port", "database.name",
		"database.user", "database.password", "database.sslmode", "database.sslrootcert",
		"database.max_conns", "database.min_conns",
		"database.max_conn_lifetime", "database.max_conn_idle_time",
		"storage.backend", "storage.endpoint", "storage.region", "storage.bucket",
		"storage.access_key", "storage.secret_key", "storage.use_ssl",
		"storage.path_style", "storage.path",
		"storage.run_artifact_retention", "storage.soft_delete_retention",
		"agent.runner", "agent.binary", "agent.base_url", "agent.effort",
		"agent.model", "agent.analyze_model", "agent.fallback_model",
		"agent.timeout", "agent.analyze_budget_usd", "agent.page_budget_usd",
		"agent.run_budget_usd", "agent.max_pages_per_run", "agent.warn_turns",
		"worker.concurrency", "worker.sweep_interval", "worker.sweep_jitter",
	}
	for _, k := range keys {
		if err := v.BindEnv(k); err != nil {
			return fmt.Errorf("config: bind %s: %w", k, err)
		}
	}
	return nil
}
