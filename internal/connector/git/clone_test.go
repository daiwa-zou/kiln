package git

import (
	"context"
	"net"
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
