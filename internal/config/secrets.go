package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// secretSpec maps a logical secret to its environment variable. Each also
// accepts a _FILE suffix naming a path to read instead, so Docker secrets and
// mounted Kubernetes secrets work without exposing values in the environment.
type secretSpec struct {
	env    string
	target *string
}

func loadSecrets(v *viper.Viper) (Secrets, error) {
	var s Secrets

	specs := []secretSpec{
		{"MASTER_KEY", &s.MasterKey},
		{"ANTHROPIC_API_KEY", &s.AnthropicAPIKey},
		{"SESSION_SECRET", &s.SessionSecret},
		{"GITHUB_CLIENT_SECRET", &s.GitHubClientSecret},
		{"GITHUB_PRIVATE_KEY", &s.GitHubPrivateKey},
		{"GITHUB_WEBHOOK_SECRET", &s.GitHubWebhookSecret},
		{"STORAGE_SECRET_KEY", &s.StorageSecretKey},
	}

	for _, spec := range specs {
		val, err := readSecret(spec.env)
		if err != nil {
			return Secrets{}, err
		}
		*spec.target = val
	}

	// database.password may also arrive via _FILE.
	if pw, err := readSecret("DATABASE_PASSWORD"); err != nil {
		return Secrets{}, err
	} else if pw != "" {
		v.Set("database.password", pw)
	}
	if dsn, err := readSecret("DATABASE_URL"); err != nil {
		return Secrets{}, err
	} else if dsn != "" {
		v.Set("database.url", dsn)
	}

	return s, nil
}

// readSecret resolves KILN_<name>, preferring KILN_<name>_FILE when both are
// set so a mounted secret always wins over an inherited environment value.
func readSecret(name string) (string, error) {
	fileVar := fmt.Sprintf("%s_%s_FILE", EnvPrefix, name)
	if path := strings.TrimSpace(os.Getenv(fileVar)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("config: read %s from %s: %w", fileVar, path, err)
		}
		// Trailing newlines are near-universal in secret files and are never
		// part of the value.
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(fmt.Sprintf("%s_%s", EnvPrefix, name)), nil
}
