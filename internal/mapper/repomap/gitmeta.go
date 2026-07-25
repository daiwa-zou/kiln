package repomap

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// recentCommitLimit bounds history reads. These repositories are small, and a
// wiki only needs enough history to describe what is changing lately.
const (
	recentCommitLimit = 30
	churnCommitLimit  = 200
	churnTopN         = 25
)

// ReadGitMeta returns repository metadata, or nil when the directory is not a
// git repository.
//
// A nil result is a supported case, not a failure: two of the directories this
// was built against have no .git at all, so everything downstream branches on
// GitMeta being absent rather than assuming history exists.
func ReadGitMeta(root string) (*GitMeta, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, nil
	}

	repo, err := git.PlainOpen(root)
	if err != nil {
		// A malformed .git should degrade to hash-based change detection
		// rather than failing the whole scan.
		return nil, nil
	}

	head, err := repo.Head()
	if err != nil {
		// A repository with no commits yet.
		return &GitMeta{}, nil
	}

	meta := &GitMeta{
		HeadSHA: head.Hash().String(),
		Branch:  head.Name().Short(),
	}

	if remotes, err := repo.Remotes(); err == nil {
		for _, r := range remotes {
			if r.Config().Name == "origin" && len(r.Config().URLs) > 0 {
				meta.Remote = r.Config().URLs[0]
				break
			}
		}
	}

	if wt, err := repo.Worktree(); err == nil {
		if status, err := wt.Status(); err == nil {
			meta.Dirty = !status.IsClean()
		}
	}

	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return meta, nil
	}
	defer iter.Close()

	churn := map[string]int{}
	var count int

	_ = iter.ForEach(func(c *object.Commit) error {
		count++
		if len(meta.Recent) < recentCommitLimit {
			meta.Recent = append(meta.Recent, CommitRef{
				SHA:     c.Hash.String(),
				When:    c.Author.When,
				Author:  c.Author.Name,
				Subject: subjectOf(c.Message),
			})
		}
		if count <= churnCommitLimit {
			accumulateChurn(c, churn)
		}
		if count >= churnCommitLimit {
			return object.ErrCanceled
		}
		return nil
	})

	meta.CommitCount = count
	meta.Churn = topChurn(churn, churnTopN)
	return meta, nil
}

// accumulateChurn counts how many commits touched each file.
//
// A root commit has no parent, so its file list comes from the tree itself.
// Diffing against a nonexistent parent silently yields nothing, which is how a
// single-commit repository ends up looking empty.
func accumulateChurn(c *object.Commit, churn map[string]int) {
	parent, err := c.Parent(0)
	if err != nil {
		tree, err := c.Tree()
		if err != nil {
			return
		}
		_ = tree.Files().ForEach(func(f *object.File) error {
			churn[f.Name]++
			return nil
		})
		return
	}

	patch, err := parent.Patch(c)
	if err != nil {
		return
	}
	for _, fp := range patch.FilePatches() {
		from, to := fp.Files()
		switch {
		case to != nil:
			churn[to.Path()]++
		case from != nil:
			churn[from.Path()]++
		}
	}
}

func topChurn(churn map[string]int, n int) []ChurnRec {
	if len(churn) == 0 {
		return nil
	}
	out := make([]ChurnRec, 0, len(churn))
	for path, commits := range churn {
		out = append(out, ChurnRec{Path: path, Commits: commits})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Commits != out[j].Commits {
			return out[i].Commits > out[j].Commits
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func subjectOf(message string) string {
	for i, r := range message {
		if r == '\n' {
			return message[:i]
		}
	}
	return message
}
