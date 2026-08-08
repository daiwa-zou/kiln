package git

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRemoteURLPolicy(t *testing.T) {
	reject := map[string]string{
		"http":        "http://example.com/repo.git",
		"git-scheme":  "git://example.com/repo.git",
		"ssh":         "ssh://git@example.com/repo.git",
		"file":        "file:///etc",
		"scp-like":    "git@github.com:foo/bar.git",
		"credentials": "https://user:pass@example.com/repo.git",
		"empty-host":  "https:///repo.git",
		"localhost":   "https://localhost/repo.git",
		"loopback-ip": "https://127.0.0.1/repo.git",
		"private-ip":  "https://10.0.0.8/repo.git",
		"metadata":    "https://169.254.169.254/latest/meta-data",
		"unspecified": "https://0.0.0.0/repo.git",
	}
	for name, raw := range reject {
		if _, err := ValidateRemoteURL(raw); err == nil {
			t.Errorf("%s: ValidateRemoteURL(%q) accepted", name, raw)
		}
	}
}

func TestValidateRemoteURLAcceptsPublicHTTPS(t *testing.T) {
	// Needs DNS; skip offline rather than fail.
	if _, err := net.LookupIP("github.com"); err != nil {
		t.Skip("no DNS; skipping public-host acceptance check")
	}
	if _, err := ValidateRemoteURL("https://github.com/torvalds/linux.git"); err != nil {
		t.Errorf("public https rejected: %v", err)
	}
}

func TestCloneShallowRefusesUnvalidatedURL(t *testing.T) {
	// CloneShallow must re-check rather than trust its caller: handing it a
	// forbidden URL directly fails before any process is spawned.
	err := CloneShallow(context.Background(), CloneOptions{
		URL: "https://127.0.0.1/repo.git", Dir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "public address") {
		t.Errorf("CloneShallow on loopback = %v, want policy rejection", err)
	}
}

func TestWriteAskpass(t *testing.T) {
	near := filepath.Join(t.TempDir(), "clone-dst")
	if err := os.MkdirAll(near, 0o755); err != nil {
		t.Fatal(err)
	}

	script, cleanup, err := writeAskpass(near)
	if err != nil {
		t.Fatalf("writeAskpass: %v", err)
	}

	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("script missing: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("script mode = %o, want 0700: a readable askpass leaks how the token is used", info.Mode().Perm())
	}

	body, _ := os.ReadFile(script)
	// The whole point of an askpass helper: the token itself must never be in
	// the script, only the environment variable that carries it.
	if strings.Contains(string(body), "ghp_") || !strings.Contains(string(body), "KILN_GIT_TOKEN") {
		t.Errorf("askpass body = %q", body)
	}
	if !strings.Contains(string(body), "x-access-token") {
		t.Errorf("askpass does not answer the username prompt: %q", body)
	}

	cleanup()
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Error("cleanup left the askpass script on disk")
	}
}

func TestDirSize(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "a"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(root, "sub", "b"), make([]byte, 50), 0o644)

	got, err := dirSize(root)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if got != 150 {
		t.Errorf("dirSize = %d, want 150", got)
	}

	if _, err := dirSize(filepath.Join(root, "missing")); err == nil {
		t.Error("dirSize succeeded on a missing directory")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("  fatal: repository not found\nhint: try again\n"); got != "fatal: repository not found" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("single"); got != "single" {
		t.Errorf("firstLine single = %q", got)
	}
}
