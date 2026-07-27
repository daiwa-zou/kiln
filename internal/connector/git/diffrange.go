package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// Commit-range change detection: the derivation diff.ChangeSet's doc comment
// always promised. Failure at any step is reported as (nil, false) rather
// than an error, because the caller's fallback — FullRebuild plus the hash
// gate — is always correct, just less surgical. Ranges are an optimization;
// they must never be a way to fail a build.

const gitCmdTimeout = 30 * time.Second

// DiffRange derives the changes between two commits in a checkout. ok is
// false when the range is unusable (unknown refs, shallow history that
// deepening could not fix, not a git directory).
func DiffRange(ctx context.Context, dir, fromRef, toRef string) (changes []diff.Change, ok bool) {
	if fromRef == "" || toRef == "" {
		return nil, false
	}
	if !EnsureRef(ctx, dir, fromRef) || !EnsureRef(ctx, dir, toRef) {
		return nil, false
	}

	out, err := gitOutput(ctx, dir, "diff", "--name-status", "--no-renames", fromRef, toRef)
	if err != nil {
		return nil, false
	}

	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		status, path, found := strings.Cut(line, "\t")
		if !found || path == "" {
			continue
		}
		var kind diff.ChangeKind
		switch {
		case strings.HasPrefix(status, "A"):
			kind = diff.Added
		case strings.HasPrefix(status, "D"):
			kind = diff.Deleted
		default: // M, T (type change), and anything exotic reads as modified
			kind = diff.Modified
		}
		changes = append(changes, diff.Change{Path: path, Kind: kind})
	}
	return changes, true
}

// HeadRef returns the checkout's current commit, or "" when there is none.
func HeadRef(ctx context.Context, dir string) string {
	out, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// EnsureRef makes a commit resolvable in the checkout, deepening a shallow
// clone in bounded steps and finally attempting a direct fetch of the commit
// (which GitHub's allow-any-SHA upload-pack supports). False means the
// caller should fall back to a full rebuild.
func EnsureRef(ctx context.Context, dir, ref string) bool {
	if hasCommit(ctx, dir, ref) {
		return true
	}
	// Deepen in two bounded steps: most webhook ranges are a handful of
	// commits, and an unbounded deepen would quietly re-download the history
	// the shallow clone existed to avoid.
	for _, deepen := range []string{"100", "1000"} {
		if _, err := gitOutput(ctx, dir, "fetch", "--deepen", deepen); err != nil {
			break // not shallow, or no remote: nothing more to deepen
		}
		if hasCommit(ctx, dir, ref) {
			return true
		}
	}
	if _, err := gitOutput(ctx, dir, "fetch", "origin", ref); err == nil && hasCommit(ctx, dir, ref) {
		return true
	}
	return false
}

func hasCommit(ctx context.Context, dir, ref string) bool {
	_, err := gitOutput(ctx, dir, "cat-file", "-e", ref+"^{commit}")
	return err == nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
