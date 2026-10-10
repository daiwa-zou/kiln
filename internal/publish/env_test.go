package publish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Publishing pushes to a remote the operator named and runs git against a
// checkout of it, so git must not inherit the parent's KILN_* secrets. The
// push token is the one deliberate exception, and only when there is one.
func TestPublishGitRunsWithoutTheParentsSecrets(t *testing.T) {
	bin := t.TempDir()
	dump := filepath.Join(bin, "env.txt")
	script := "#!/bin/sh\nenv > '" + dump + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KILN_MASTER_KEY", "master-secret")
	t.Setenv("KILN_SESSION_SECRET", "session-secret")

	dir := t.TempDir()
	env, cleanup, err := gitEnv(dir, "push-token")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	_, _ = runGit(context.Background(), dir, env, gitAuthor{}, "status")

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake git never ran: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, kv := range got {
		if strings.HasPrefix(kv, "KILN_") && kv != "KILN_GIT_TOKEN=push-token" {
			t.Errorf("secret leaked into git's environment: %s", kv)
		}
	}
	for _, want := range []string{"KILN_GIT_TOKEN=push-token", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_LFS_SKIP_SMUDGE=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("git's environment is missing %s", want)
		}
	}
}
