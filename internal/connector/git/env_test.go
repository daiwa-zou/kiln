package git

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGitDumpingEnv puts a "git" first on PATH that records the environment it
// was started with and then fails. The children here parse someone else's
// repository, so what matters is what they are handed, not what they do.
func fakeGitDumpingEnv(t *testing.T) (dump string) {
	t.Helper()
	bin := t.TempDir()
	dump = filepath.Join(bin, "env.txt")
	script := "#!/bin/sh\nenv > '" + dump + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KILN_MASTER_KEY", "master-secret")
	t.Setenv("KILN_SESSION_SECRET", "session-secret")
	return dump
}

func readDump(t *testing.T, dump string) []string {
	t.Helper()
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake git never ran: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestCloneRunsGitWithoutTheParentsSecrets(t *testing.T) {
	dump := fakeGitDumpingEnv(t)

	// A literal public address needs no DNS to pass the URL policy.
	_ = CloneShallow(context.Background(), CloneOptions{
		URL: "https://8.8.8.8/repo.git", Dir: t.TempDir(), Token: "clone-token",
	})

	env := readDump(t, dump)
	for _, kv := range env {
		if strings.HasPrefix(kv, "KILN_") && kv != "KILN_GIT_TOKEN=clone-token" {
			t.Errorf("secret leaked into git's environment: %s", kv)
		}
	}
	for _, want := range []string{"KILN_GIT_TOKEN=clone-token", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_LFS_SKIP_SMUDGE=1", "GIT_CONFIG_NOSYSTEM=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("git's environment is missing %s", want)
		}
	}
}

func TestGitOutputRunsGitWithoutTheParentsSecrets(t *testing.T) {
	dump := fakeGitDumpingEnv(t)

	_, _ = gitOutput(context.Background(), t.TempDir(), "", localCmdTimeout, "status")

	for _, kv := range readDump(t, dump) {
		if strings.HasPrefix(kv, "KILN_") {
			t.Errorf("secret leaked into git's environment: %s", kv)
		}
	}
}
