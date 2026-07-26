package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAllowedPathDeniesEverythingWithoutRoots(t *testing.T) {
	dir := t.TempDir()
	if _, err := AllowedPath(dir, nil); err == nil {
		t.Fatal("empty allowlist permitted a path; it must deny by default")
	}
}

func TestAllowedPathAcceptsInsideRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "repo")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := AllowedPath(sub, []string{root})
	if err != nil {
		t.Fatalf("path inside root rejected: %v", err)
	}
	// The resolved form must still be inside the root.
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if !strings.HasPrefix(got, resolvedRoot) {
		t.Errorf("resolved path %q escaped root %q", got, resolvedRoot)
	}

	// The root itself is also permitted.
	if _, err := AllowedPath(root, []string{root}); err != nil {
		t.Errorf("root itself rejected: %v", err)
	}
}

func TestAllowedPathRejectsOutsideAndRelatives(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	if _, err := AllowedPath(outside, []string{root}); err == nil {
		t.Error("path outside root accepted")
	}
	if _, err := AllowedPath("relative/path", []string{root}); err == nil {
		t.Error("relative path accepted; it would resolve against the worker's cwd")
	}
	// A sibling whose name shares the root as a string prefix must not pass a
	// naive prefix check: /srv/repos-evil is not under /srv/repos.
	sibling := root + "-evil"
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sibling)
	if _, err := AllowedPath(sibling, []string{root}); err == nil {
		t.Error("string-prefix sibling accepted")
	}
}

func TestAllowedPathResolvesSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	target := t.TempDir() // outside the root

	link := filepath.Join(root, "escape")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// The link lives under the root, but its resolution does not: it must be
	// rejected, or a permitted directory becomes an escape hatch.
	if _, err := AllowedPath(link, []string{root}); err == nil {
		t.Error("symlink escaping the root was accepted")
	}
}

func TestAllowedPathRejectsMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := AllowedPath(filepath.Join(root, "nope"), []string{root}); err == nil {
		t.Error("nonexistent path accepted")
	}
}
