package repomap

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// gitFixture builds a real repository in a temp dir using go-git, committing
// each file map in order. No shelling out, no global git config dependency.
func gitFixture(t *testing.T, commits ...map[string]string) (string, *git.Repository) {
	t.Helper()
	root := t.TempDir()

	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	when := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for i, files := range commits {
		for rel, content := range files {
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := wt.Add(rel); err != nil {
				t.Fatalf("add %s: %v", rel, err)
			}
		}
		when = when.Add(time.Hour)
		if _, err := wt.Commit(
			"commit "+string(rune('A'+i))+"\n\nbody detail line\n",
			&git.CommitOptions{Author: &object.Signature{Name: "Ada", Email: "ada@example.test", When: when}},
		); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	return root, repo
}

func TestReadGitMetaNonRepoIsNilNotError(t *testing.T) {
	meta, err := ReadGitMeta(t.TempDir())
	if err != nil || meta != nil {
		t.Errorf("plain directory = (%v, %v), want (nil, nil)", meta, err)
	}

	// A malformed .git degrades to hash-based change detection, not a failure.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gibberish"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err = ReadGitMeta(dir)
	if err != nil || meta != nil {
		t.Errorf("malformed .git = (%v, %v), want (nil, nil)", meta, err)
	}
}

func TestReadGitMetaEmptyRepo(t *testing.T) {
	root := t.TempDir()
	if _, err := git.PlainInit(root, false); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadGitMeta(root)
	if err != nil {
		t.Fatalf("ReadGitMeta: %v", err)
	}
	// No commits yet: an empty struct, not nil -- downstream branches on
	// presence, and the repo does exist.
	if meta == nil || meta.HeadSHA != "" || meta.CommitCount != 0 {
		t.Errorf("empty repo meta = %+v, want zero-valued struct", meta)
	}
}

func TestReadGitMetaSingleRootCommitHasChurn(t *testing.T) {
	// The parent-less root commit is the documented trap: diffing against a
	// nonexistent parent silently yields nothing, so churn must come from the
	// tree itself.
	root, _ := gitFixture(t, map[string]string{
		"main.go":     "package main\n",
		"lib/util.go": "package lib\n",
	})

	meta, err := ReadGitMeta(root)
	if err != nil {
		t.Fatalf("ReadGitMeta: %v", err)
	}
	if meta.CommitCount != 1 {
		t.Fatalf("commit count = %d, want 1", meta.CommitCount)
	}
	if len(meta.Churn) != 2 {
		t.Fatalf("churn = %+v, want both files from the root tree", meta.Churn)
	}
	if meta.HeadSHA == "" || len(meta.Recent) != 1 {
		t.Errorf("head/recent not populated: %+v", meta)
	}
	if meta.Recent[0].Subject != "commit A" {
		t.Errorf("subject = %q, want first line only", meta.Recent[0].Subject)
	}
	if meta.Dirty {
		t.Error("freshly committed worktree reported dirty")
	}
}

func TestReadGitMetaChurnRemoteAndDirty(t *testing.T) {
	root, repo := gitFixture(t,
		map[string]string{"main.go": "v1\n", "README.md": "one\n"},
		map[string]string{"main.go": "v2\n"},
		map[string]string{"main.go": "v3\n", "lib/util.go": "new\n"},
	)
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{"https://example.test/repo.git"},
	}); err != nil {
		t.Fatal(err)
	}
	// An uncommitted change makes the tree dirty.
	if err := os.WriteFile(filepath.Join(root, "scratch.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := ReadGitMeta(root)
	if err != nil {
		t.Fatalf("ReadGitMeta: %v", err)
	}
	if meta.CommitCount != 3 {
		t.Errorf("commit count = %d, want 3", meta.CommitCount)
	}
	if meta.Remote != "https://example.test/repo.git" {
		t.Errorf("remote = %q", meta.Remote)
	}
	if !meta.Dirty {
		t.Error("worktree with an untracked file reported clean")
	}
	// main.go was touched by all three commits and must rank first.
	if len(meta.Churn) == 0 || meta.Churn[0].Path != "main.go" || meta.Churn[0].Commits != 3 {
		t.Errorf("top churn = %+v, want main.go x3", meta.Churn)
	}
	// Recent is newest-first: commit C is HEAD.
	if meta.Recent[0].Subject != "commit C" {
		t.Errorf("newest subject = %q, want commit C", meta.Recent[0].Subject)
	}
}

func TestTopChurnOrdersAndTruncates(t *testing.T) {
	churn := map[string]int{"b.go": 2, "a.go": 2, "hot.go": 9, "cold.go": 1}

	out := topChurn(churn, 3)
	if len(out) != 3 {
		t.Fatalf("len = %d, want truncated to 3", len(out))
	}
	if out[0].Path != "hot.go" {
		t.Errorf("first = %s, want the hottest file", out[0].Path)
	}
	// Equal counts tie-break by path so output is deterministic.
	if out[1].Path != "a.go" || out[2].Path != "b.go" {
		t.Errorf("tie order = %s, %s; want a.go then b.go", out[1].Path, out[2].Path)
	}
	if topChurn(map[string]int{}, 5) != nil {
		t.Error("empty churn should be nil")
	}
}

func TestSubjectOf(t *testing.T) {
	if got := subjectOf("first line\nsecond"); got != "first line" {
		t.Errorf("subjectOf = %q", got)
	}
	if got := subjectOf("no newline"); got != "no newline" {
		t.Errorf("subjectOf without newline = %q", got)
	}
}
