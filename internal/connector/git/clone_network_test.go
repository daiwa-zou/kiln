package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCloneShallowPublicRepo proves the clone path end-to-end against a real
// public remote. Gated on KILN_TEST_NETWORK so the default suite stays
// offline, matching how database tests key off KILN_TEST_DATABASE_URL.
func TestCloneShallowPublicRepo(t *testing.T) {
	if os.Getenv("KILN_TEST_NETWORK") == "" {
		t.Skip("KILN_TEST_NETWORK not set; skipping network clone test")
	}

	dir := t.TempDir()
	err := CloneShallow(context.Background(), CloneOptions{
		URL: "https://github.com/octocat/Hello-World.git", Dir: dir,
	})
	if err != nil {
		t.Fatalf("CloneShallow: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("clone produced no .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Errorf("clone produced no working tree: %v", err)
	}
}
