package childproc

import (
	"slices"
	"strings"
	"testing"
)

func setSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("KILN_MASTER_KEY", "master-secret")
	t.Setenv("KILN_SESSION_SECRET", "session-secret")
	t.Setenv("KILN_DATABASE_URL", "postgres://u:pw@db/kiln")
	t.Setenv("ANTHROPIC_API_KEY", "sk-parent")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
}

func assertNoSecrets(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		for _, bad := range []string{"KILN_", "ANTHROPIC_", "AWS_"} {
			if strings.HasPrefix(kv, bad) {
				t.Errorf("secret leaked into the child environment: %s", kv)
			}
		}
	}
}

func TestEnvKeepsOnlyWhatTheBinaryNeeds(t *testing.T) {
	setSecrets(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/kiln")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("TMPDIR", "/tmp/kiln")

	env := Env()
	assertNoSecrets(t, env)
	for _, want := range []string{
		"PATH=/usr/bin:/bin", "HOME=/home/kiln", "LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8", "TMPDIR=/tmp/kiln",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env is missing %s: %v", want, env)
		}
	}
}

func TestEnvAppendsDeliberateExtras(t *testing.T) {
	setSecrets(t)
	env := Env("KILN_GIT_TOKEN=tok")
	if !slices.Contains(env, "KILN_GIT_TOKEN=tok") {
		t.Errorf("a variable passed on purpose was dropped: %v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "KILN_") && kv != "KILN_GIT_TOKEN=tok" {
			t.Errorf("inherited secret: %s", kv)
		}
	}
}

func TestGitEnvDisablesHostConfigAndKeepsNetworkSettings(t *testing.T) {
	setSecrets(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/corp.pem")
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_SSH_COMMAND", "sh -c evil")

	env := GitEnv("GIT_ASKPASS=/tmp/askpass.sh")
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_LFS_SKIP_SMUDGE=1", "GIT_ASKPASS=/tmp/askpass.sh",
		"HTTPS_PROXY=http://proxy.internal:3128", "SSL_CERT_FILE=/etc/ssl/corp.pem",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env is missing %s: %v", want, env)
		}
	}
	assertNoSecrets(t, env)
	// Other GIT_* variables steer where git works and what it runs; only the
	// ones set above are meant to reach it.
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			t.Errorf("inherited git steering variable: %s", kv)
		}
	}
}
